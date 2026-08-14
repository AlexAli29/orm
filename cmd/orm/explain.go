package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"slices"
	"strings"
	"text/tabwriter"

	"github.com/AlexAli29/orm/internal/gen"
	"github.com/AlexAli29/orm/internal/gen/config"
	"github.com/AlexAli29/orm/internal/gen/model"
)

// orm explain prints what reconciliation proved about one entity.
//
// It answers the questions people actually ask of a mapping: which column is
// this field, can it be null, will an insert have to supply it, which
// constraint backs this relation and which way round does it point. All of that
// is already known — it is what generation is produced from — and the only
// thing missing was somewhere to read it.
//
// It proves nothing new and runs no query against user data. Everything printed
// comes from the same reconciliation orm check runs.

func explain(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("orm explain", flag.ContinueOnError)
	fs.SetOutput(stderr)
	path := fs.String("config", "orm.yaml", "path to the configuration file")
	asJSON := fs.Bool("json", false, "print the mapping as JSON")
	asSQL := fs.Bool("sql", false, "print the statement a plain query over the entity compiles to")
	fs.Usage = func() {
		fmt.Fprint(stderr, "usage: orm explain <entity> [flags]\n\n"+
			"  <entity> is the struct's name, or package/path.Name when one name is\n"+
			"  declared in more than one configured package.\n\n")
		fs.PrintDefaults()
	}
	// The entity is written where it reads best — "orm explain User --json" —
	// which is not where the flag package expects it, since it stops at the
	// first argument that is not a flag. Lifting it out first means both
	// orderings work rather than one of them being a silent parse failure.
	name, rest, err := takeOperand(args)
	if err != nil {
		fmt.Fprintf(stderr, "orm explain: %v\n", err)
		fs.Usage()
		return exitFailure
	}
	if err := fs.Parse(rest); err != nil {
		return exitFailure
	}
	switch {
	case fs.NArg() > 0:
		fmt.Fprintf(stderr, "orm explain: unexpected argument %q\n", fs.Arg(0))
		return exitFailure
	case *asJSON && *asSQL:
		fmt.Fprintln(stderr, "orm explain: --json and --sql ask for different things; pick one")
		return exitFailure
	}

	cfg, err := config.Load(*path)
	if err != nil {
		fmt.Fprintf(stderr, "orm explain: %v\n", err)
		return exitFailure
	}
	result, err := gen.Check(ctx, cfg)
	if err != nil {
		fmt.Fprintf(stderr, "orm explain: %v\n", err)
		return exitFailure
	}

	em, err := findEntity(result.Mapping, name)
	if err != nil {
		fmt.Fprintf(stderr, "orm explain: %v\n", err)
		return exitFailure
	}

	switch {
	case *asJSON:
		err = explainJSON(stdout, em)
	case *asSQL:
		err = explainSQL(stdout, em)
	default:
		err = explainText(stdout, em)
	}
	if err != nil {
		fmt.Fprintf(stderr, "orm explain: %v\n", err)
		return exitFailure
	}
	return exitClean
}

// takeOperand pulls the single non-flag argument out of a command line,
// wherever it was written, and returns the flags around it.
//
// Only the first bare word is taken: a second one is a mistake worth reporting
// rather than a name to guess between. A value that follows a flag stays with
// it, so "--config orm.yaml User" reads the way it looks.
func takeOperand(args []string) (string, []string, error) {
	operand, rest, err := splitOperand(args)
	if err != nil {
		return "", nil, err
	}
	if operand == "" {
		return "", nil, errors.New("name an entity")
	}
	return operand, rest, nil
}

// splitOperand is takeOperand for a command whose operand is optional.
func splitOperand(args []string) (string, []string, error) {
	var (
		operand string
		rest    = make([]string, 0, len(args))
		taken   bool
	)
	for i := 0; i < len(args); i++ {
		a := args[i]
		if strings.HasPrefix(a, "-") {
			rest = append(rest, a)
			// A flag written as "--config path" consumes the word after it;
			// one written as "--config=path" or "--json" does not.
			if !strings.Contains(a, "=") && wantsValue(a) && i+1 < len(args) {
				i++
				rest = append(rest, args[i])
			}
			continue
		}
		if taken {
			return "", nil, fmt.Errorf("unexpected argument %q", a)
		}
		operand, taken = a, true
	}
	return operand, rest, nil
}

// valueFlags names every flag in this command that takes the word after it.
//
// It is one set for the whole binary rather than one per subcommand, because
// the only thing it is used for is deciding whether a bare word is an operand
// or a flag's value — and a flag named in one command and not another would
// make the same command line parse two ways.
var valueFlags = map[string]bool{
	"config":       true,
	"format":       true,
	"fail-on":      true,
	"name":         true,
	"rename":       true,
	"rename-table": true,
	"out":          true,
}

// wantsValue reports whether a flag takes the word after it.
func wantsValue(flag string) bool { return valueFlags[strings.TrimLeft(flag, "-")] }

// findEntity resolves a name against the proved mapping.
//
// A bare name is accepted when it is unambiguous. When it is not, the candidates
// are listed rather than one being chosen: picking would mean printing a
// confident description of the wrong type.
func findEntity(m *model.Mapping, name string) (*model.EntityMapping, error) {
	if m == nil {
		return nil, fmt.Errorf("nothing was reconciled")
	}
	var (
		exact    []*model.EntityMapping
		byName   []*model.EntityMapping
		allNames []string
	)
	for _, em := range m.Entities {
		qualified := em.Entity.PkgPath + "." + em.Entity.Name
		allNames = append(allNames, qualified)
		switch {
		case qualified == name:
			exact = append(exact, em)
		case em.Entity.Name == name:
			byName = append(byName, em)
		}
	}
	switch {
	case len(exact) == 1:
		return exact[0], nil
	case len(byName) == 1:
		return byName[0], nil
	case len(byName) > 1:
		qualified := make([]string, 0, len(byName))
		for _, em := range byName {
			qualified = append(qualified, em.Entity.PkgPath+"."+em.Entity.Name)
		}
		slices.Sort(qualified)
		return nil, fmt.Errorf("%q is declared in more than one configured package; name one of:\n  %s",
			name, strings.Join(qualified, "\n  "))
	}
	slices.Sort(allNames)
	return nil, fmt.Errorf("no entity named %q; this configuration maps:\n  %s", name, strings.Join(allNames, "\n  "))
}

func explainText(w io.Writer, em *model.EntityMapping) error {
	fmt.Fprintf(w, "Entity: %s\n", em.Entity.Name)
	fmt.Fprintf(w, "Go:     %s.%s\n", em.Entity.PkgPath, em.Entity.Name)
	fmt.Fprintf(w, "Table:  %s\n", em.Table.Qualified())

	fmt.Fprint(w, "\nColumns\n\n")
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprint(tw, "  Go field\tGo type\tPostgreSQL\tProperties\n")
	for _, cm := range em.Cols {
		fmt.Fprintf(tw, "  %s\t%s\t%s %s\t%s\n",
			cm.Field.Name, cm.Field.Type.Src, cm.Column.Name, typeOf(cm.Column.Type),
			strings.Join(properties(em, cm.Column), " "))
	}
	if err := tw.Flush(); err != nil {
		return err
	}

	if len(em.Rels) == 0 {
		return nil
	}
	fmt.Fprint(w, "\nRelations\n")
	for _, rel := range em.Rels {
		fmt.Fprintf(w, "\n  %s %s\n", rel.Field.Name, rel.Field.Type.Src)
		fmt.Fprintf(w, "    direction:  %s\n", direction(rel))
		fmt.Fprintf(w, "    constraint: %s\n", rel.FK.Name)
		for i := range rel.KeyCols {
			from := em.Table.Qualified() + "." + rel.KeyCols[i].Column.Name
			to := rel.Target.Table.Qualified() + "." + rel.TargetCols[i].Column.Name
			mapped := ""
			if !rel.KeyCols[i].Mapped() {
				mapped = "  (not mapped to a Go field; read from the statement that loads the parents)"
			}
			fmt.Fprintf(w, "    key:        %s -> %s%s\n", from, to, mapped)
		}
		fmt.Fprintf(w, "    target:     %s.%s\n", rel.Target.Entity.PkgPath, rel.Target.Entity.Name)
	}
	return nil
}

// direction says which table carries the foreign key and how many rows the
// relation can hold, which together are the two things people get wrong.
func direction(rel model.RelMapping) string {
	side := "the target's table carries the foreign key"
	shape := "has-many"
	if rel.FKSide == model.FKLocal {
		side = "this table carries the foreign key"
		shape = "belongs-to"
	} else if rel.Cardinality == model.CardOne {
		shape = "has-one"
	}
	return fmt.Sprintf("%s (%s)", shape, side)
}

func properties(em *model.EntityMapping, col *model.PGColumn) []string {
	var out []string
	if slices.Contains(em.Table.PK, col) {
		out = append(out, "PK")
	}
	if !col.Nullable() {
		out = append(out, "NOT NULL")
	} else {
		out = append(out, "nullable")
	}
	switch col.Identity {
	case 'a':
		out = append(out, "identity (always)")
	case 'd':
		out = append(out, "identity (by default)")
	}
	if col.Generated != 0 {
		out = append(out, "generated")
	}
	if col.HasDefault {
		out = append(out, "default")
	}
	return out
}

func typeOf(t *model.PGType) string {
	if t == nil {
		return "?"
	}
	if t.Schema == "pg_catalog" {
		return t.Name
	}
	return t.Schema + "." + t.Name
}

// explainSQL prints the statement a query with no conditions compiles to, which
// is also the column order [orm.Raw] has to return.
func explainSQL(w io.Writer, em *model.EntityMapping) error {
	var b strings.Builder
	b.WriteString("SELECT\n")
	for i, cm := range em.Cols {
		fmt.Fprintf(&b, "    %q.%q", em.Table.Name, cm.Column.Name)
		if i < len(em.Cols)-1 {
			b.WriteByte(',')
		}
		b.WriteByte('\n')
	}
	fmt.Fprintf(&b, "FROM %q.%q\n", em.Table.Schema, em.Table.Name)
	_, err := io.WriteString(w, b.String())
	return err
}

// explainEntity is the JSON shape. It is a type of its own rather than the
// model's, because the model is internal and free to change while this is
// something a script may read.
type explainEntity struct {
	Entity    string            `json:"entity"`
	Package   string            `json:"package"`
	Table     string            `json:"table"`
	Schema    string            `json:"schema"`
	Columns   []explainColumn   `json:"columns"`
	Relations []explainRelation `json:"relations,omitempty"`
}

type explainColumn struct {
	Field      string `json:"field"`
	GoType     string `json:"go_type"`
	Column     string `json:"column"`
	PGType     string `json:"pg_type"`
	PrimaryKey bool   `json:"primary_key"`
	NotNull    bool   `json:"not_null"`
	HasDefault bool   `json:"has_default"`
	Identity   bool   `json:"identity"`
	Generated  bool   `json:"generated"`
}

type explainRelation struct {
	Field       string          `json:"field"`
	GoType      string          `json:"go_type"`
	Cardinality string          `json:"cardinality"`
	ForeignKey  string          `json:"foreign_key"`
	KeyOnEntity bool            `json:"key_on_entity"`
	Target      string          `json:"target"`
	TargetTable string          `json:"target_table"`
	Keys        []explainRelKey `json:"keys"`
}

type explainRelKey struct {
	From   string `json:"from"`
	To     string `json:"to"`
	Mapped bool   `json:"mapped"`
}

func explainJSON(w io.Writer, em *model.EntityMapping) error {
	out := explainEntity{
		Entity:  em.Entity.Name,
		Package: em.Entity.PkgPath,
		Table:   em.Table.Name,
		Schema:  em.Table.Schema,
	}
	for _, cm := range em.Cols {
		out.Columns = append(out.Columns, explainColumn{
			Field:      cm.Field.Name,
			GoType:     cm.Field.Type.Src,
			Column:     cm.Column.Name,
			PGType:     typeOf(cm.Column.Type),
			PrimaryKey: slices.Contains(em.Table.PK, cm.Column),
			NotNull:    !cm.Column.Nullable(),
			HasDefault: cm.Column.HasDefault,
			Identity:   cm.Column.Identity != 0,
			Generated:  cm.Column.Generated != 0,
		})
	}
	for _, rel := range em.Rels {
		r := explainRelation{
			Field:       rel.Field.Name,
			GoType:      rel.Field.Type.Src,
			Cardinality: rel.Cardinality.String(),
			ForeignKey:  rel.FK.Name,
			KeyOnEntity: rel.FKSide == model.FKLocal,
			Target:      rel.Target.Entity.PkgPath + "." + rel.Target.Entity.Name,
			TargetTable: rel.Target.Table.Qualified(),
		}
		for i := range rel.KeyCols {
			r.Keys = append(r.Keys, explainRelKey{
				From:   em.Table.Qualified() + "." + rel.KeyCols[i].Column.Name,
				To:     rel.Target.Table.Qualified() + "." + rel.TargetCols[i].Column.Name,
				Mapped: rel.KeyCols[i].Mapped(),
			})
		}
		out.Relations = append(out.Relations, r)
	}

	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(out)
}
