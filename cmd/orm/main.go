// Command orm reconciles hand-written Go entity structs against a PostgreSQL
// schema.
//
//	orm init                 write a starting orm.yaml
//	orm check                report every disagreement between the structs and the schema
//	orm generate             emit typed table descriptors, metadata and scanners
//	orm explain <entity>     print what reconciliation proved about one entity
//	orm inspect              print the live schema in canonical form
//	orm version              print the version
//
// You own your structs. PostgreSQL owns your schema. This command proves they
// agree.
//
// In managed mode the Go declarations own the schema instead, and migrations
// carry it to PostgreSQL:
//
//	orm makemigrations       write the migration the models ask for
//	orm migrate              apply migrations to the database
//	orm showmigrations       list the migrations and which are applied
//	orm sqlmigrate <id>      print the SQL one migration runs
//
// Both modes end in the same place: a proven mapping and generated code. What
// differs is who decides what the schema is.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"runtime/debug"
	"strings"
	"syscall"

	"github.com/AlexAli29/orm/gen/diag"
	"github.com/AlexAli29/orm/internal/gen"
	"github.com/AlexAli29/orm/internal/gen/config"
	"github.com/AlexAli29/orm/internal/gen/lock"
	"github.com/AlexAli29/orm/internal/gen/managed"
)

// version is set at link time by a release build. It is empty otherwise, which
// is the ordinary case: the version then comes from Go's own build information,
// so `go install ...@v0.1.0` reports v0.1.0 without anything being stamped by
// hand. A constant baked in here would go stale the moment a tag moved past it.
var version string

// Version reports the tool's version, and where it came from.
//
// A build from a working tree has no version to report — Go calls that
// "(devel)" — so the VCS revision the toolchain stamped is used instead. That is
// the difference between "which release is this" and "which commit is this",
// and both are worth being able to answer.
func Version() string {
	if version != "" {
		return version
	}
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return "(unknown)"
	}
	v := info.Main.Version
	if v != "" && v != "(devel)" {
		return v
	}
	var revision, modified string
	for _, s := range info.Settings {
		switch s.Key {
		case "vcs.revision":
			revision = s.Value
		case "vcs.modified":
			if s.Value == "true" {
				modified = "-dirty"
			}
		}
	}
	if revision == "" {
		return "(devel)"
	}
	if len(revision) > 12 {
		revision = revision[:12]
	}
	return "(devel " + revision + modified + ")"
}

// Exit codes. They are part of the command's contract: a build that treats a
// configuration mistake as a reconciliation failure would report a broken
// pipeline as broken code.
const (
	// exitClean means no finding reached the failure threshold.
	exitClean = 0
	// exitFindings means reconciliation produced findings at or above it.
	exitFindings = 1
	// exitFailure means the tool could not run: bad configuration, unreachable
	// database, packages that do not compile.
	exitFailure = 2
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	os.Exit(runIO(ctx, os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}

// run is runIO with nothing to read from.
//
// Only one command asks a question, and it refuses to guess when there is
// nobody to answer, so a caller with no input to supply is a caller running
// everything else.
func run(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	return runIO(ctx, args, strings.NewReader(""), stdout, stderr)
}

func runIO(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		usage(stderr)
		return exitFailure
	}

	cmd, rest := args[0], args[1:]
	switch cmd {
	case "check":
		return check(ctx, rest, stdout, stderr)
	case "init":
		return initConfig(rest, stdout, stderr)
	case "version":
		fmt.Fprintf(stdout, "orm %s\n", Version())
		return exitClean
	case "generate":
		return generate(ctx, rest, stdout, stderr)
	case "explain":
		return explain(ctx, rest, stdout, stderr)
	case "makemigrations":
		return makemigrations(ctx, rest, stdin, stdout, stderr)
	case "migrate":
		return migrateCmd(ctx, rest, stdout, stderr)
	case "showmigrations":
		return showmigrations(ctx, rest, stdout, stderr)
	case "sqlmigrate":
		return sqlmigrate(rest, stdout, stderr)
	case "inspect":
		return inspect(ctx, rest, stdout, stderr)
	case "help", "-h", "--help":
		usage(stdout)
		return exitClean
	default:
		fmt.Fprintf(stderr, "orm: unknown command %q\n\n", cmd)
		usage(stderr)
		return exitFailure
	}
}

func usage(w io.Writer) {
	fmt.Fprint(w, `orm reconciles Go entity structs against a PostgreSQL schema.

usage:
  orm init [flags]              write a starting orm.yaml
  orm check [flags]             report every disagreement between the structs and the schema
  orm generate [flags]          emit typed table descriptors, metadata and scanners
  orm explain <entity> [flags]  print what reconciliation proved about one entity
  orm inspect [flags]           print the live schema in canonical form
  orm version                   print the version

managed schema mode, where the Go declarations own the schema:
  orm makemigrations [flags]        write the migration the models ask for
  orm migrate [target] [flags]      apply migrations to the database
  orm showmigrations [flags]        list the migrations and which are applied
  orm sqlmigrate <migration>        print the SQL one migration runs

run "orm <command> -h" for a command's flags.

exit codes:
  0  no finding reached the failure threshold
  1  findings at or above the threshold, including stale generated code, pending
     migrations, database drift, and a "makemigrations --check" that needs a migration
  2  the tool could not run: bad configuration, unreachable database, no entities,
     a package that does not compile, or an entity orm explain could not resolve
`)
}

// renderers maps a --format value to its writer.
var renderers = map[string]func(io.Writer, *diag.Report) error{
	"text":   diag.RenderText,
	"json":   diag.RenderJSON,
	"github": diag.RenderGitHub,
}

// formatNames lists the formats in a fixed order, because map iteration order
// is random and an error message must not be.
var formatNames = []string{"text", "json", "github"}

func check(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("orm check", flag.ContinueOnError)
	fs.SetOutput(stderr)
	path := fs.String("config", "orm.yaml", "path to the configuration file")
	format := fs.String("format", "text", "report format: text, json or github")
	failOn := fs.String("fail-on", "error", "lowest severity that fails the command: error or warning")
	generated := fs.Bool("generated", false, "also fail when the generated code is stale or has never been written")
	fs.Usage = func() {
		fmt.Fprint(stderr, "usage: orm check [flags]\n\n")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return exitFailure
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(stderr, "orm check: unexpected argument %q\n", fs.Arg(0))
		return exitFailure
	}

	render, ok := renderers[*format]
	if !ok {
		fmt.Fprintf(stderr, "orm check: invalid format %q, want %s\n", *format, strings.Join(formatNames, ", "))
		return exitFailure
	}
	threshold, err := diag.ParseSeverity(*failOn)
	if err != nil {
		fmt.Fprintf(stderr, "orm check: --fail-on: %v\n", err)
		return exitFailure
	}

	cfg, err := config.Load(*path)
	if err != nil {
		fmt.Fprintf(stderr, "orm check: %v\n", err)
		return exitFailure
	}

	result, err := gen.Check(ctx, cfg)
	if err != nil {
		if errors.Is(err, context.Canceled) {
			fmt.Fprintln(stderr, "orm check: cancelled")
			return exitFailure
		}
		fmt.Fprintf(stderr, "orm check: %v\n", err)
		return exitFailure
	}

	if err := render(stdout, result.Report); err != nil {
		fmt.Fprintf(stderr, "orm check: %v\n", err)
		return exitFailure
	}
	mappingFailed := result.Report.Failed(threshold)

	// The mapping holds. Whether the committed generated code still describes
	// it is a second question, and one the compiler cannot ask.
	state, err := generationState(*path, result)
	if err != nil {
		fmt.Fprintf(stderr, "orm check: %v\n", err)
		return exitFailure
	}

	// In managed mode the mapping is one of four things worth knowing, and the
	// other three are about migrations. In database mode there are no
	// migrations to report on, and a project that never asked for them must not
	// start failing because it has none.
	if cfg.Schema.Mode == config.ModeManaged {
		mapping := "valid"
		if mappingFailed {
			mapping = fmt.Sprintf("%d finding(s)", len(result.Report.Findings()))
		}
		report, err := managedState(ctx, managed.Open(cfg), mapping, mappingFailed, state, *generated)
		if err != nil {
			fmt.Fprintf(stderr, "orm check: %v\n", err)
			return exitFailure
		}
		// The state block is for a person. A machine-readable format put it on
		// standard output would produce a document that is no longer the format
		// it claims to be, so it goes beside the report rather than into it.
		out := stdout
		if *format != "text" {
			out = stderr
		}
		report.write(out)
		if report.Failed {
			return exitFindings
		}
		return exitClean
	}

	if mappingFailed {
		return exitFindings
	}
	if reportGeneration(stdout, state, *generated) && *generated {
		return exitFindings
	}
	return exitClean
}

// generationState compares the mapping just proved against the one the
// generated code was produced from.
func generationState(configPath string, result *gen.Result) (lock.State, error) {
	f, present, err := lock.Read(lock.Path(configPath))
	if err != nil {
		return 0, err
	}
	return lock.Compare(f, present, lock.Fingerprint(result.Mapping)), nil
}

// reportGeneration prints what was found and reports whether it is a problem.
//
// Missing is not a problem unless asked about: a project that has never
// generated is a project mid-setup, and failing its first check would make the
// tool hostile to the workflow it recommends.
func reportGeneration(w io.Writer, state lock.State, asked bool) bool {
	switch state {
	case lock.Stale:
		fmt.Fprint(w, "\ngenerated code is stale: it was produced from a different mapping\n\n"+
			"    run: orm generate\n")
		return true
	case lock.Unknown:
		fmt.Fprintf(w, "\n%s was written by a different version of this tool\n\n"+
			"    run: orm generate\n", lock.Name)
		return true
	case lock.Missing:
		if asked {
			fmt.Fprintf(w, "\nno %s: this project has not generated code yet\n\n"+
				"    run: orm generate\n", lock.Name)
		}
		return true
	default:
		return false
	}
}

func generate(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("orm generate", flag.ContinueOnError)
	fs.SetOutput(stderr)
	path := fs.String("config", "orm.yaml", "path to the configuration file")
	failOn := fs.String("fail-on", "error", "lowest severity that blocks generation: error or warning")
	dry := fs.Bool("dry-run", false, "report what would be written without writing it")
	fs.Usage = func() {
		fmt.Fprint(stderr, "usage: orm generate [flags]\n\n")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return exitFailure
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(stderr, "orm generate: unexpected argument %q\n", fs.Arg(0))
		return exitFailure
	}

	threshold, err := diag.ParseSeverity(*failOn)
	if err != nil {
		fmt.Fprintf(stderr, "orm generate: --fail-on: %v\n", err)
		return exitFailure
	}

	cfg, err := config.Load(*path)
	if err != nil {
		fmt.Fprintf(stderr, "orm generate: %v\n", err)
		return exitFailure
	}

	// In managed mode the schema has an owner, and generating against a
	// database that owner has not approved is how a runtime ends up describing
	// something nobody wrote down.
	if cfg.Schema.Mode == config.ModeManaged {
		if err := preflight(ctx, managed.Open(cfg)); err != nil {
			fmt.Fprintf(stderr, "orm generate: %v\n", err)
			return exitFindings
		}
	}

	result, files, err := gen.Generate(ctx, cfg, threshold)
	switch {
	case errors.Is(err, gen.ErrUnproven):
		// The structs and the schema disagree, so there is nothing to generate
		// from. Show the author what stopped it, in the same form orm check
		// would have, and write nothing.
		fmt.Fprintln(stderr, "orm generate: the entities and the schema do not agree; nothing was written")
		if err := diag.RenderText(stderr, result.Report); err != nil {
			fmt.Fprintf(stderr, "orm generate: %v\n", err)
			return exitFailure
		}
		return exitFindings
	case err != nil:
		fmt.Fprintf(stderr, "orm generate: %v\n", err)
		return exitFailure
	}

	if *dry {
		for _, f := range files {
			fmt.Fprintf(stdout, "would write %s (%d bytes)\n", cfg.Rel(f.Path), len(f.Content))
		}
		fmt.Fprintf(stdout, "would write %s (%s)\n", cfg.Rel(lock.Path(*path)), lock.Fingerprint(result.Mapping))
		return exitClean
	}
	if err := gen.Write(files); err != nil {
		fmt.Fprintf(stderr, "orm generate: %v\n", err)
		return exitFailure
	}
	// The lock is written last and only once every file landed, so it never
	// claims a generation that did not finish.
	lockPath := lock.Path(*path)
	if err := lock.Write(lockPath, lock.Fingerprint(result.Mapping)); err != nil {
		fmt.Fprintf(stderr, "orm generate: %v\n", err)
		return exitFailure
	}
	for _, f := range files {
		fmt.Fprintf(stdout, "wrote %s\n", cfg.Rel(f.Path))
	}
	fmt.Fprintf(stdout, "wrote %s\n", cfg.Rel(lockPath))
	return exitClean
}

func initConfig(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("orm init", flag.ContinueOnError)
	fs.SetOutput(stderr)
	path := fs.String("config", "orm.yaml", "path to write")
	force := fs.Bool("force", false, "overwrite an existing file")
	mode := fs.String("mode", "database", "who owns the schema: database or managed")
	fs.Usage = func() {
		fmt.Fprint(stderr, "usage: orm init [flags]\n\n")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return exitFailure
	}
	m, err := config.ParseMode(*mode)
	if err != nil {
		fmt.Fprintf(stderr, "orm init: --mode: %v\n", err)
		return exitFailure
	}
	template := config.Template
	if m == config.ModeManaged {
		template = config.ManagedTemplate
	}

	if _, err := os.Stat(*path); err == nil && !*force {
		fmt.Fprintf(stderr, "orm init: %s already exists; pass --force to overwrite it\n", *path)
		return exitFailure
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		fmt.Fprintf(stderr, "orm init: %v\n", err)
		return exitFailure
	}

	if err := os.WriteFile(*path, []byte(template), 0o644); err != nil {
		fmt.Fprintf(stderr, "orm init: %v\n", err)
		return exitFailure
	}
	fmt.Fprintf(stdout, "wrote %s\n", *path)
	return exitClean
}
