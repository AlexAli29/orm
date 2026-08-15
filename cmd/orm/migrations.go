package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"

	"github.com/AlexAli29/orm/internal/gen/config"
	"github.com/AlexAli29/orm/internal/gen/managed"
	"github.com/AlexAli29/orm/internal/gen/migrate"
	"github.com/AlexAli29/orm/internal/gen/schema"
	"github.com/jackc/pgx/v5"
)

// The migration commands.
//
// They are thin on purpose: every decision they make is one of "which state do
// I need" and "what do I print", and the states themselves come from services
// that know nothing about a terminal. The one thing that genuinely belongs here
// is the question — whether a column that disappeared and one that appeared are
// the same column — because that is the only thing in the whole system that
// cannot be computed and has to be asked.

// openProject loads a configuration and prepares the managed services.
func openProject(path string) (*managed.Project, error) {
	cfg, err := config.Load(path)
	if err != nil {
		return nil, err
	}
	return managed.Open(cfg), nil
}

// requireManaged refuses a command that only means something in managed mode.
func requireManaged(p *managed.Project) error {
	if p.Managed() {
		return nil
	}
	return fmt.Errorf("%w\n\n    this project is in database mode: PostgreSQL owns the schema and something else maintains it", managed.ErrNotManaged)
}

// connect opens the connection a command runs against, and returns a closer.
func connect(ctx context.Context, p *managed.Project) (*pgx.Conn, func(), error) {
	conn, err := p.Connect(ctx)
	if err != nil {
		return nil, nil, err
	}
	return conn, func() { _ = conn.Close(context.WithoutCancel(ctx)) }, nil
}

// renameFlag collects the renames a caller resolved on the command line.
//
// It exists so that a rename can be confirmed without a terminal. Without it,
// the only ways to get a rename past CI would be to run the command by hand and
// commit the result, or to let the tool guess — and guessing is exactly what
// this whole path is built to avoid.
type renameFlag struct {
	renames []migrate.Rename
	table   bool
}

func (f *renameFlag) String() string { return "" }

func (f *renameFlag) Set(v string) error {
	from, to, ok := strings.Cut(v, "=")
	if !ok || from == "" || to == "" {
		if f.table {
			return fmt.Errorf("write --rename-table old=new")
		}
		return fmt.Errorf("write --rename table.old=new")
	}
	if f.table {
		schemaName, name := splitQualified(from)
		f.renames = append(f.renames, migrate.Rename{Schema: schemaName, From: name, To: to})
		return nil
	}
	table, column, ok := strings.Cut(from, ".")
	if !ok {
		return fmt.Errorf("write --rename table.old=new; %q names no table", from)
	}
	// A three-part name qualifies the table with its schema.
	schemaName := "public"
	if rest, col, ok := strings.Cut(column, "."); ok {
		schemaName, table, column = table, rest, col
	}
	f.renames = append(f.renames, migrate.Rename{Schema: schemaName, Table: table, From: column, To: to})
	return nil
}

func splitQualified(s string) (schemaName, name string) {
	if a, b, ok := strings.Cut(s, "."); ok {
		return a, b
	}
	return "public", s
}

func makemigrations(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("orm makemigrations", flag.ContinueOnError)
	fs.SetOutput(stderr)
	path := fs.String("config", "orm.yaml", "path to the configuration file")
	name := fs.String("name", "", "descriptive half of the migration's ID")
	checkOnly := fs.Bool("check", false, "report whether a migration is needed and write nothing; exit 1 if one is")
	dryRun := fs.Bool("dry-run", false, "show the migration without writing it")
	showSQL := fs.Bool("sql", false, "with --dry-run, also print the SQL the migration would run")
	noInput := fs.Bool("no-input", false, "never ask a question; fail instead when a rename cannot be resolved")
	noRename := fs.Bool("no-rename", false, "treat every rename candidate as a drop and an add")
	baseline := fs.Bool("baseline", false, "build the first migration from the schema the database already has")
	empty := fs.Bool("empty", false, "write a migration with a raw SQL operation to fill in, for data the schema diff cannot see")
	renames := &renameFlag{}
	tableRenames := &renameFlag{table: true}
	fs.Var(renames, "rename", "a confirmed column rename, as table.old=new; repeatable")
	fs.Var(tableRenames, "rename-table", "a confirmed table rename, as old=new; repeatable")
	fs.Usage = func() {
		fmt.Fprint(stderr, "usage: orm makemigrations [flags]\n\n")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return exitFailure
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(stderr, "orm makemigrations: unexpected argument %q\n", fs.Arg(0))
		return exitFailure
	}

	p, err := openProject(*path)
	if err != nil {
		fmt.Fprintf(stderr, "orm makemigrations: %v\n", err)
		return exitFailure
	}
	// --baseline is the one thing a database-first project may do here, and it
	// is how it stops being one: it reads the schema that already exists and
	// writes the migration describing it. Nothing is applied, and the mode is
	// only worth switching once that artifact is in hand.
	if *baseline {
		if *checkOnly {
			fmt.Fprint(stderr, "orm makemigrations: --baseline writes the migration that describes an existing database,"+
				" and --check asks whether one is needed; they are different questions\n")
			return exitFailure
		}
		return makeBaseline(ctx, p, *name, *dryRun, stdout, stderr)
	}
	if err := requireManaged(p); err != nil {
		fmt.Fprintf(stderr, "orm makemigrations: %v\n", err)
		return exitFailure
	}

	set, err := p.Set()
	if err != nil {
		fmt.Fprintf(stderr, "orm makemigrations: %v\n", err)
		return exitFailure
	}
	state, err := set.State()
	if err != nil {
		fmt.Fprintf(stderr, "orm makemigrations: %v\n", err)
		return exitFailure
	}
	want, err := p.Desired(ctx)
	if err != nil {
		fmt.Fprintf(stderr, "orm makemigrations: %v\n", err)
		return exitFailure
	}

	opts := migrate.Options{Renames: append(slices.Clone(tableRenames.renames), renames.renames...)}
	// What the planner needs to prove a view replacement safe. Reaching the
	// database is optional: without it, replacements are refused rather than
	// guessed at.
	cfg := p.Config()
	opts.Views = viewPlanInput(ctx, cfg.Schema.DSN, cfg.Schema.SearchPath)
	d, err := migrate.Compute(state, want, opts)
	if err != nil {
		fmt.Fprintf(stderr, "orm makemigrations: %v\n", err)
		return exitFailure
	}

	// A candidate is a question, and a question only needs asking when it
	// changes the answer. --check is deciding whether a migration is needed at
	// all, which no rename changes.
	if len(d.Candidates) > 0 && !*noRename && !*checkOnly {
		confirmed, err := resolveRenames(d.Candidates, opts.Renames, *noInput, stdin, stdout)
		if err != nil {
			fmt.Fprintf(stderr, "orm makemigrations: %v\n", err)
			return exitFailure
		}
		if len(confirmed) > len(opts.Renames) {
			opts.Renames = confirmed
			if d, err = migrate.Compute(state, want, opts); err != nil {
				fmt.Fprintf(stderr, "orm makemigrations: %v\n", err)
				return exitFailure
			}
		}
	}

	// An empty migration is asked for rather than derived, so it is written
	// whether or not the models moved. Data is the case the diff cannot see:
	// a backfill, a seed, a correction. The operation is a stub with the SQL
	// left blank, because inventing SQL nobody asked for would be worse than
	// an obvious hole.
	if *empty {
		name := *name
		if name == "" {
			// Summarize has no operations to read, and "auto" would name a
			// migration nothing derived it. Data is what --empty is for.
			name = "data"
		}
		id := migrate.NextID(set, name)
		m := &migrate.Migration{
			ID:     id,
			Atomic: true,
			Operations: []migrate.Operation{migrate.RawSQL{
				// A stub that refuses to run. An empty statement is rejected
				// when the migration is written, and a comment would apply
				// cleanly and do nothing — which is the worst of the three,
				// because a migration that forgot its contents would be
				// recorded as applied. This one fails until it is edited.
				Up: "DO $$ BEGIN\n" +
					"    RAISE EXCEPTION 'this migration was created with --empty and never filled in';\n" +
					"END $$;",
				Atomic:      true,
				Description: "describe what this does",
			}},
		}
		if last := set.Migrations(); len(last) > 0 {
			m.DependsOn = []string{last[len(last)-1].ID}
		}
		if *dryRun {
			reportMigration(stdout, m, true)
			fmt.Fprintf(stdout, "\n--dry-run: %s was not written\n", p.Config().Rel(p.Store().Path(id)))
			return exitClean
		}
		file, err := p.Store().Write(m)
		if err != nil {
			fmt.Fprintf(stderr, "orm makemigrations: %v\n", err)
			return exitFailure
		}
		fmt.Fprintf(stdout, "wrote %s\n\n", p.Config().Rel(file))
		fmt.Fprint(stdout, "Fill in Up with the SQL to run, and Down with the SQL that undoes it.\n"+
			"Leaving Down empty makes the migration irreversible, which is the honest\n"+
			"answer when it is. If the SQL changes the schema rather than the data,\n"+
			"add a state_only operation beside it so the migration state stays true.\n")
		return exitClean
	}

	if d.Empty() {
		fmt.Fprintln(stdout, "No schema changes detected.")
		return exitClean
	}

	if *checkOnly {
		fmt.Fprint(stdout, "The models describe schema changes no migration does.\n\n")
		fmt.Fprint(stdout, migrate.RenderSummary(d.Operations, ""))
		if len(d.Candidates) > 0 {
			fmt.Fprintf(stdout, "\n%d of these may be renames rather than a drop and an add:\n", len(d.Candidates))
			for _, c := range d.Candidates {
				fmt.Fprintf(stdout, "    %s\n", c)
			}
		}
		fmt.Fprint(stdout, "\nRun:\n    orm makemigrations\n")
		return exitFindings
	}

	id := migrate.NextID(set, chooseName(*name, d.Operations))
	m := &migrate.Migration{ID: id, Operations: d.Operations, Atomic: true}
	if last := set.Migrations(); len(last) > 0 {
		m.DependsOn = []string{last[len(last)-1].ID}
	}
	for _, op := range d.Operations {
		if !op.Transactional() {
			m.Atomic = false
		}
	}

	// Everything that can refuse the migration refuses it now, before a word of
	// summary is printed: an enum label that PostgreSQL cannot remove, an
	// operation with no representation in a file.
	if err := migrate.Editable(m); err != nil {
		fmt.Fprintf(stderr, "orm makemigrations: this change cannot be migrated automatically:\n\n    %v\n", err)
		return exitFailure
	}

	reportMigration(stdout, m, *dryRun)
	if *showSQL && *dryRun {
		if err := writeSQL(stdout, m.Operations); err != nil {
			fmt.Fprintf(stderr, "orm makemigrations: %v\n", err)
			return exitFailure
		}
	}
	if *dryRun {
		fmt.Fprintf(stdout, "\n--dry-run: %s was not written\n", p.Config().Rel(p.Store().Path(id)))
		return exitClean
	}

	file, err := p.Store().Write(m)
	if err != nil {
		fmt.Fprintf(stderr, "orm makemigrations: %v\n", err)
		return exitFailure
	}
	fmt.Fprintf(stdout, "\nwrote %s\n", p.Config().Rel(file))
	return exitClean
}

// reportMigration prints the summary a person reviews before the migration runs.
func reportMigration(w io.Writer, m *migrate.Migration, dry bool) {
	verb := "Migration"
	if dry {
		verb = "Would write migration"
	}
	fmt.Fprintf(w, "%s %s", verb, m.ID)
	if !m.Atomic {
		fmt.Fprint(w, " [non-atomic]")
	}
	fmt.Fprint(w, "\n\n")
	fmt.Fprint(w, migrate.RenderSummary(m.Operations, ""))

	warnings := migrate.Warnings(m.Operations)
	if !m.Atomic {
		warnings = append([]migrate.Warning{{
			Code:      migrate.WNonAtomic,
			Message:   "this migration runs outside a transaction: an operation that fails part way through leaves the ones before it applied",
			Operation: m.ID,
		}}, warnings...)
	}
	if s := migrate.RenderWarnings(warnings, ""); s != "" {
		fmt.Fprint(w, "\n"+s)
	}
}

// chooseName picks the descriptive half of a migration ID.
//
// A supplied name always wins. Without one it is derived from the operations,
// deterministically — the same change produces the same name, so two people who
// run the command on one branch get one migration rather than two that differ
// only in what the clock said.
func chooseName(supplied string, ops []migrate.Operation) string {
	if supplied != "" {
		return supplied
	}
	changes := migrate.Summarize(ops)
	groups := make([]string, 0, len(changes))
	for _, c := range changes {
		if c.Group != "" && !slices.Contains(groups, c.Group) {
			groups = append(groups, c.Group)
		}
	}
	switch {
	case len(groups) == 1:
		return groups[0]
	case len(groups) > 1 && len(groups) <= 3:
		return strings.Join(groups, "_")
	default:
		return "auto"
	}
}

// resolveRenames asks about each candidate the caller has not already answered.
func resolveRenames(candidates []migrate.RenameCandidate, already []migrate.Rename, noInput bool, stdin io.Reader, stdout io.Writer) ([]migrate.Rename, error) {
	pending := make([]migrate.RenameCandidate, 0, len(candidates))
	for _, c := range candidates {
		if slices.ContainsFunc(already, func(r migrate.Rename) bool {
			return r.Schema == c.Schema && r.Table == c.Table && (r.From == c.From || r.To == c.To)
		}) {
			continue
		}
		pending = append(pending, c)
	}
	if len(pending) == 0 {
		return already, nil
	}

	if noInput {
		var b strings.Builder
		b.WriteString("these changes could be renames or could be a drop and an add, and the difference is whether the data survives:\n")
		for _, c := range pending {
			fmt.Fprintf(&b, "\n    %s\n        %s\n", c, c.Reason)
			if c.Table == "" {
				fmt.Fprintf(&b, "        confirm with:  --rename-table %s=%s\n", c.From, c.To)
			} else {
				fmt.Fprintf(&b, "        confirm with:  --rename %s.%s=%s\n", c.Table, c.From, c.To)
			}
		}
		b.WriteString("\nor pass --no-rename to say they are not renames, which drops the old objects and their data.")
		return nil, errors.New(b.String())
	}

	confirmed := slices.Clone(already)
	answered := make(map[string]bool)
	reader := bufio.NewReader(stdin)
	for _, c := range pending {
		// One object is renamed to one other object. Once a side is spoken for,
		// the remaining candidates naming it are answered.
		if answered[c.Schema+"."+c.Table+"."+c.From] || answered[c.Schema+"."+c.Table+">"+c.To] {
			continue
		}
		yes, err := ask(reader, stdout, renamePrompt(c))
		if err != nil {
			return nil, err
		}
		if !yes {
			continue
		}
		confirmed = append(confirmed, migrate.Rename{Schema: c.Schema, Table: c.Table, From: c.From, To: c.To})
		answered[c.Schema+"."+c.Table+"."+c.From] = true
		answered[c.Schema+"."+c.Table+">"+c.To] = true
	}
	return confirmed, nil
}

// renamePrompt is the question asked about one candidate.
//
// A table rename is asked more carefully than a column one. A wrong answer
// about a column loses that column; a wrong answer about a table loses the
// table and everything in it, and two tables with the same shape are far more
// likely to be two different tables than two spellings of one.
func renamePrompt(c migrate.RenameCandidate) string {
	if c.Table == "" {
		return fmt.Sprintf("Did you rename the table %s to %s?\n"+
			"    %s.\n"+
			"    Answering yes moves the existing table and its rows; answering no drops %s and creates %s empty.",
			schema.ShortName(c.Schema, c.From), c.To, c.Reason, c.From, c.To)
	}
	return fmt.Sprintf("Did you rename %s.%s to %s?\n    %s.",
		schema.ShortName(c.Schema, c.Table), c.From, c.To, c.Reason)
}

// ask puts a yes/no question and reads the answer. The default is no: the
// destructive reading of a candidate is the one the engine already computed,
// and silence is not consent to rewrite it.
func ask(r *bufio.Reader, w io.Writer, question string) (bool, error) {
	fmt.Fprintf(w, "\n%s [y/N] ", question)
	line, err := r.ReadString('\n')
	if err != nil && line == "" {
		if errors.Is(err, io.EOF) {
			return false, errors.New("there is nobody to answer the question; run the command in a terminal," +
				" or resolve the rename with --rename, --rename-table or --no-rename")
		}
		return false, err
	}
	answer := strings.ToLower(strings.TrimSpace(line))
	return answer == "y" || answer == "yes", nil
}

// makeBaseline writes the migration that describes a schema the database
// already has.
//
// This is the one place a migration is computed from a live database rather
// than from the models, and it is the one place where that is the right answer:
// the question being asked is not "what should the schema be" but "what is it
// already", so that everything after this point can be an increment.
func makeBaseline(ctx context.Context, p *managed.Project, name string, dry bool, stdout, stderr io.Writer) int {
	set, err := p.Set()
	if err != nil {
		fmt.Fprintf(stderr, "orm makemigrations: %v\n", err)
		return exitFailure
	}
	if set.Len() > 0 {
		fmt.Fprintf(stderr, "orm makemigrations: --baseline is for a project with no migrations, and this one has %d;"+
			" a baseline describes a schema nothing in the history explains\n", set.Len())
		return exitFailure
	}

	conn, closeConn, err := connect(ctx, p)
	if err != nil {
		fmt.Fprintf(stderr, "orm makemigrations: %v\n", err)
		return exitFailure
	}
	defer closeConn()

	actual, err := p.Actual(ctx, conn)
	if err != nil {
		fmt.Fprintf(stderr, "orm makemigrations: %v\n", err)
		return exitFailure
	}
	if name == "" {
		name = "initial"
	}
	m, err := managed.Baseline(migrate.NextID(set, name), actual)
	if err != nil {
		fmt.Fprintf(stderr, "orm makemigrations: %v\n", err)
		return exitFailure
	}
	if err := migrate.Editable(m); err != nil {
		fmt.Fprintf(stderr, "orm makemigrations: %v\n", err)
		return exitFailure
	}

	reportMigration(stdout, m, dry)
	if dry {
		fmt.Fprintf(stdout, "\n--dry-run: %s was not written\n", p.Config().Rel(p.Store().Path(m.ID)))
		return exitClean
	}
	file, err := p.Store().Write(m)
	if err != nil {
		fmt.Fprintf(stderr, "orm makemigrations: %v\n", err)
		return exitFailure
	}
	fmt.Fprintf(stdout, "\nwrote %s\n\n", p.Config().Rel(file))
	fmt.Fprint(stdout, "This migration describes the schema the database already has, so it must not be run.\n")
	if !p.Managed() {
		fmt.Fprint(stdout, "\nSet schema.mode: managed in the configuration, then record it as applied:\n")
	} else {
		fmt.Fprint(stdout, "\nRecord it as applied instead:\n")
	}
	fmt.Fprintf(stdout, "\n    orm migrate --fake %s\n", m.ID)
	return exitClean
}

func migrateCmd(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("orm migrate", flag.ContinueOnError)
	fs.SetOutput(stderr)
	path := fs.String("config", "orm.yaml", "path to the configuration file")
	plan := fs.Bool("plan", false, "show what would run and change nothing")
	fake := fs.Bool("fake", false, "record the target as applied without running its SQL")
	force := fs.Bool("force", false, "with --fake, record it even though the database does not match")
	fs.Usage = func() {
		fmt.Fprint(stderr, "usage: orm migrate [target] [flags]\n\n"+
			"the target is a migration ID or a unique prefix of one; the default is the latest.\n\n")
		fs.PrintDefaults()
	}
	// The target is written where it reads best — "orm migrate 0007 --plan" —
	// which is not where the flag package expects it, since it stops at the
	// first argument that is not a flag.
	arg, rest, err := splitOperand(args)
	if err != nil {
		fmt.Fprintf(stderr, "orm migrate: %v\n", err)
		return exitFailure
	}
	if err := fs.Parse(rest); err != nil {
		return exitFailure
	}

	p, err := openProject(*path)
	if err != nil {
		fmt.Fprintf(stderr, "orm migrate: %v\n", err)
		return exitFailure
	}
	if err := requireManaged(p); err != nil {
		fmt.Fprintf(stderr, "orm migrate: %v\n", err)
		return exitFailure
	}
	set, err := p.Set()
	if err != nil {
		fmt.Fprintf(stderr, "orm migrate: %v\n", err)
		return exitFailure
	}
	if set.Len() == 0 {
		fmt.Fprintf(stdout, "There are no migrations in %s.\n\nRun:\n    orm makemigrations\n",
			p.Config().Rel(p.Store().Dir()))
		return exitClean
	}

	target := ""
	if arg != "" {
		if target, err = resolveTarget(set, arg); err != nil {
			fmt.Fprintf(stderr, "orm migrate: %v\n", err)
			return exitFailure
		}
	}

	conn, closeConn, err := connect(ctx, p)
	if err != nil {
		fmt.Fprintf(stderr, "orm migrate: %v\n", err)
		return exitFailure
	}
	defer closeConn()

	m := migrate.New(conn, set)
	switch {
	case *fake:
		return fakeMigrate(ctx, p, m, set, conn, target, *force, stdout, stderr)
	case *plan:
		return showPlan(ctx, m, set, target, stdout, stderr)
	}

	m.Watch(
		func(s migrate.Step) {
			verb := "Applying"
			if s.Direction == migrate.Reverse {
				verb = "Reversing"
			}
			fmt.Fprintf(stdout, "%s %s ... ", verb, s.Migration.ID)
		},
		func(_ migrate.Step, err error) {
			if err != nil {
				fmt.Fprintln(stdout, "FAILED")
				return
			}
			fmt.Fprintln(stdout, "OK")
		},
	)

	done, err := m.Migrate(ctx, target)
	if err != nil {
		fmt.Fprintf(stderr, "\norm migrate: %v\n", err)
		return exitFailure
	}
	if done.Empty() {
		fmt.Fprintln(stdout, "The database is up to date.")
		return exitClean
	}
	fmt.Fprintf(stdout, "\n%d migration(s) applied.\n", len(done.Steps))
	return exitClean
}

// resolveTarget turns an argument into a migration ID.
//
// A prefix is accepted because "0007" is what a person types and what a plan
// prints next to; an ambiguous one is refused rather than resolved to the first
// match, since the two candidates are different schemas.
func resolveTarget(set *migrate.Set, arg string) (string, error) {
	if _, ok := set.Get(arg); ok {
		return arg, nil
	}
	var matches []string
	for _, m := range set.Migrations() {
		if strings.HasPrefix(m.ID, arg) {
			matches = append(matches, m.ID)
		}
	}
	switch len(matches) {
	case 1:
		return matches[0], nil
	case 0:
		return "", fmt.Errorf("no migration named %q", arg)
	default:
		return "", fmt.Errorf("%q names %d migrations: %s", arg, len(matches), strings.Join(matches, ", "))
	}
}

func showPlan(ctx context.Context, m *migrate.Migrator, set *migrate.Set, target string, stdout, stderr io.Writer) int {
	applied, err := appliedIDs(ctx, m)
	if err != nil {
		fmt.Fprintf(stderr, "orm migrate: %v\n", err)
		return exitFailure
	}
	plan, err := m.Plan(ctx, target)
	if err != nil {
		fmt.Fprintf(stderr, "orm migrate: %v\n", err)
		return exitFailure
	}

	current := "nothing applied"
	if len(applied) > 0 {
		current = applied[len(applied)-1]
	}
	wanted := target
	if wanted == "" {
		all := set.Migrations()
		wanted = all[len(all)-1].ID
	}
	fmt.Fprintf(stdout, "current: %s\ntarget:  %s\n", current, wanted)
	if plan.Empty() {
		fmt.Fprint(stdout, "\nThe database is up to date.\n")
		return exitClean
	}
	fmt.Fprintf(stdout, "\n%d migration(s) to run %s:\n", len(plan.Steps), plan.Direction)
	for _, s := range plan.Steps {
		mode := "atomic"
		if !s.Atomic {
			mode = "non-atomic"
		}
		fmt.Fprintf(stdout, "\n%s [%s]\n", s.Migration.ID, mode)
		fmt.Fprint(stdout, migrate.RenderSummary(s.Operations, "  "))
	}
	if s := migrate.RenderWarnings(migrate.PlanWarnings(plan), ""); s != "" {
		fmt.Fprint(stdout, "\n"+s)
	}
	return exitClean
}

func appliedIDs(ctx context.Context, m *migrate.Migrator) ([]string, error) {
	applied, err := m.Applied(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(applied))
	for _, a := range applied {
		out = append(out, a.ID)
	}
	return out, nil
}

// fakeMigrate records migrations as applied without running them.
//
// It verifies first. A fake is a claim that the database already has what the
// migration describes, and the one thing this tool can do that a person cannot
// do reliably by eye is check that claim — so it does, and only a caller who
// says --force gets to make the claim anyway.
func fakeMigrate(ctx context.Context, p *managed.Project, m *migrate.Migrator, set *migrate.Set,
	conn *pgx.Conn, target string, force bool, stdout, stderr io.Writer,
) int {
	if target == "" {
		fmt.Fprint(stderr, "orm migrate: --fake needs the migration to record, because recording every pending migration"+
			" as applied is a claim about the database that nothing here can check\n\n    orm migrate --fake 0001_initial\n")
		return exitFailure
	}

	// The plan is computed for its validation: an edited history, a target
	// behind the current state, a checksum that no longer matches.
	plan, err := m.Plan(ctx, target)
	if err != nil {
		fmt.Fprintf(stderr, "orm migrate: %v\n", err)
		return exitFailure
	}
	if plan.Direction == migrate.Reverse {
		fmt.Fprintf(stderr, "orm migrate: %s is behind the current state; --fake records migrations as applied and cannot un-apply one\n", target)
		return exitFailure
	}
	if plan.Empty() {
		fmt.Fprintf(stdout, "%s is already recorded as applied.\n", target)
		return exitClean
	}

	expected, err := set.StateAt(target)
	if err != nil {
		fmt.Fprintf(stderr, "orm migrate: %v\n", err)
		return exitFailure
	}
	actual, err := p.Actual(ctx, conn)
	if err != nil {
		fmt.Fprintf(stderr, "orm migrate: %v\n", err)
		return exitFailure
	}
	if diffs := schema.Diff(expected, actual); len(diffs) > 0 {
		if !force {
			fmt.Fprintf(stderr, "orm migrate: the database is not in the state %s describes, so recording it as applied"+
				" would make the history claim something untrue:\n\n", target)
			for _, d := range diffs {
				fmt.Fprintf(stderr, "    %s\n", d)
			}
			fmt.Fprint(stderr, "\nrun the migrations instead, or pass --force if you are certain the difference does not matter\n")
			return exitFailure
		}
		fmt.Fprintf(stdout, "--force: recording %s as applied even though the database differs from it in %d way(s).\n",
			target, len(diffs))
	}

	ids := make([]string, 0, len(plan.Steps))
	for _, s := range plan.Steps {
		ids = append(ids, s.Migration.ID)
	}
	done, err := m.MarkApplied(ctx, ids...)
	if err != nil {
		fmt.Fprintf(stderr, "orm migrate: %v\n", err)
		return exitFailure
	}
	for _, s := range done.Steps {
		fmt.Fprintf(stdout, "Faking %s ... recorded, not run\n", s.Migration.ID)
	}
	fmt.Fprintf(stdout, "\n%d migration(s) recorded as applied. No schema SQL ran.\n", len(done.Steps))
	return exitClean
}

func showmigrations(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("orm showmigrations", flag.ContinueOnError)
	fs.SetOutput(stderr)
	path := fs.String("config", "orm.yaml", "path to the configuration file")
	fs.Usage = func() {
		fmt.Fprint(stderr, "usage: orm showmigrations [flags]\n\n")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return exitFailure
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(stderr, "orm showmigrations: unexpected argument %q\n", fs.Arg(0))
		return exitFailure
	}

	p, err := openProject(*path)
	if err != nil {
		fmt.Fprintf(stderr, "orm showmigrations: %v\n", err)
		return exitFailure
	}
	if err := requireManaged(p); err != nil {
		fmt.Fprintf(stderr, "orm showmigrations: %v\n", err)
		return exitFailure
	}
	set, err := p.Set()
	if err != nil {
		fmt.Fprintf(stderr, "orm showmigrations: %v\n", err)
		return exitFailure
	}

	conn, closeConn, err := connect(ctx, p)
	if err != nil {
		fmt.Fprintf(stderr, "orm showmigrations: %v\n", err)
		return exitFailure
	}
	defer closeConn()

	m := migrate.New(conn, set)
	applied, err := m.Applied(ctx)
	if err != nil {
		fmt.Fprintf(stderr, "orm showmigrations: %v\n", err)
		return exitFailure
	}

	if set.Len() == 0 {
		fmt.Fprintf(stdout, "There are no migrations in %s.\n", p.Config().Rel(p.Store().Dir()))
	}
	byID := make(map[string]migrate.Applied, len(applied))
	for _, a := range applied {
		byID[a.ID] = a
	}
	// A migration whose artifact changed after it was applied is the one thing
	// in this listing that is not a status, and it is marked so that it cannot
	// be skimmed past.
	var problems []string
	for _, mig := range set.Migrations() {
		a, ok := byID[mig.ID]
		mark := " "
		switch {
		case !ok:
		case a.Checksum == "":
			mark = "X"
		default:
			sum, _ := set.Checksum(mig.ID)
			if a.Checksum != sum {
				mark = "!"
				problems = append(problems, fmt.Sprintf(
					"%s was modified after it was applied\n        applied:  %s\n        current:  %s",
					mig.ID, a.Checksum[:min(12, len(a.Checksum))], sum[:min(12, len(sum))]))
				break
			}
			mark = "X"
		}
		fmt.Fprintf(stdout, "[%s] %s", mark, mig.ID)
		if !mig.Atomic {
			fmt.Fprint(stdout, "  (non-atomic)")
		}
		fmt.Fprintln(stdout)
	}
	for _, a := range applied {
		if _, ok := set.Get(a.ID); !ok {
			problems = append(problems, fmt.Sprintf("%s is recorded as applied but is not in %s",
				a.ID, p.Config().Rel(p.Store().Dir())))
		}
	}
	if len(problems) > 0 {
		fmt.Fprint(stderr, "\nthe history and the migrations do not agree:\n")
		for _, p := range problems {
			fmt.Fprintf(stderr, "    %s\n", p)
		}
		fmt.Fprint(stderr, "\nhistory cannot be rewritten; write a new migration instead\n")
		return exitFindings
	}
	return exitClean
}

func sqlmigrate(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("orm sqlmigrate", flag.ContinueOnError)
	fs.SetOutput(stderr)
	path := fs.String("config", "orm.yaml", "path to the configuration file")
	reverse := fs.Bool("reverse", false, "print the SQL that would undo the migration")
	fs.Usage = func() {
		fmt.Fprint(stderr, "usage: orm sqlmigrate <migration> [flags]\n\n")
		fs.PrintDefaults()
	}
	arg, rest, err := splitOperand(args)
	if err != nil {
		fmt.Fprintf(stderr, "orm sqlmigrate: %v\n", err)
		return exitFailure
	}
	if err := fs.Parse(rest); err != nil {
		return exitFailure
	}
	if arg == "" {
		fmt.Fprint(stderr, "usage: orm sqlmigrate <migration> [flags]\n")
		return exitFailure
	}
	p, err := openProject(*path)
	if err != nil {
		fmt.Fprintf(stderr, "orm sqlmigrate: %v\n", err)
		return exitFailure
	}
	if err := requireManaged(p); err != nil {
		fmt.Fprintf(stderr, "orm sqlmigrate: %v\n", err)
		return exitFailure
	}
	set, err := p.Set()
	if err != nil {
		fmt.Fprintf(stderr, "orm sqlmigrate: %v\n", err)
		return exitFailure
	}
	id, err := resolveTarget(set, arg)
	if err != nil {
		fmt.Fprintf(stderr, "orm sqlmigrate: %v\n", err)
		return exitFailure
	}
	m, _ := set.Get(id)

	ops := m.Operations
	atomic := m.Atomic
	if *reverse {
		if ops, err = set.Reverse(id); err != nil {
			fmt.Fprintf(stderr, "orm sqlmigrate: %v\n", err)
			return exitFailure
		}
		for _, op := range ops {
			if !op.Transactional() {
				atomic = false
			}
		}
	}

	fmt.Fprintf(stdout, "-- %s", id)
	if *reverse {
		fmt.Fprint(stdout, " (reverse)")
	}
	fmt.Fprintln(stdout)
	if atomic {
		fmt.Fprintln(stdout, "-- runs in one transaction, together with the row recording it as applied")
	} else {
		fmt.Fprintln(stdout, "-- runs outside a transaction: an operation that fails leaves the ones before it applied")
	}
	if err := writeSQL(stdout, ops); err != nil {
		fmt.Fprintf(stderr, "orm sqlmigrate: %v\n", err)
		return exitFailure
	}
	return exitClean
}

// writeSQL prints exactly the statements the engine would execute.
//
// Nothing here is reformatted or approximated, and the transaction and history
// bookkeeping is deliberately not printed: it is the engine's, it is described
// in the header comment, and printing it would produce a script that records a
// migration as applied when somebody pastes it into psql to try one statement.
func writeSQL(w io.Writer, ops []migrate.Operation) error {
	for _, op := range ops {
		statements, err := op.SQL()
		if err != nil {
			return err
		}
		fmt.Fprintf(w, "\n-- %s\n", op.Describe())
		for _, s := range statements {
			fmt.Fprintf(w, "%s;\n", s)
		}
	}
	return nil
}

func inspect(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("orm inspect", flag.ContinueOnError)
	fs.SetOutput(stderr)
	path := fs.String("config", "orm.yaml", "path to the configuration file")
	asJSON := fs.Bool("json", false, "print the schema as JSON")
	out := fs.String("out", "", "write to a file instead of standard output")
	fs.Usage = func() {
		fmt.Fprint(stderr, "usage: orm inspect [flags]\n\n"+
			"prints the live schema in canonical form. It is read-only.\n\n")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return exitFailure
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(stderr, "orm inspect: unexpected argument %q\n", fs.Arg(0))
		return exitFailure
	}

	p, err := openProject(*path)
	if err != nil {
		fmt.Fprintf(stderr, "orm inspect: %v\n", err)
		return exitFailure
	}
	conn, closeConn, err := connect(ctx, p)
	if err != nil {
		fmt.Fprintf(stderr, "orm inspect: %v\n", err)
		return exitFailure
	}
	defer closeConn()

	s, err := p.Actual(ctx, conn)
	if err != nil {
		fmt.Fprintf(stderr, "orm inspect: %v\n", err)
		return exitFailure
	}

	var buf strings.Builder
	if *asJSON {
		err = managed.Render(&buf, s)
	} else {
		err = schema.Text(&buf, s)
	}
	if err != nil {
		fmt.Fprintf(stderr, "orm inspect: %v\n", err)
		return exitFailure
	}
	if *out == "" {
		fmt.Fprint(stdout, buf.String())
		return exitClean
	}
	if err := os.WriteFile(*out, []byte(buf.String()), 0o644); err != nil {
		fmt.Fprintf(stderr, "orm inspect: %v\n", err)
		return exitFailure
	}
	fmt.Fprintf(stdout, "wrote %s\n", *out)
	return exitClean
}
