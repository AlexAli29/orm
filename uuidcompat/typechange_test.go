package uuidcompat_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Changing a column's type into or out of uuid.
//
// The planner is the existing one and nothing here teaches it a uuid
// conversion. The question is only what it does when a declaration moves: plan
// the change, or refuse it. Refusing is a correct answer — PostgreSQL will not
// cast text to uuid without being told how, and a tool that invented a USING
// expression to make the migration succeed would be choosing, on the operator's
// behalf, what to do with the rows that do not convert.
//
// What must not happen either way is a destructive artifact left behind by a
// failure, or a diagnostic that differs between runs.

// typeChangeCase is one declared change and what the planner did with it.
type typeChangeCase struct {
	name string
	from string // the pgtype the column starts as
	to   string // the pgtype it is moved to
	goTy string // the Go type that goes with `to`
}

// The four transitions the qualification asks about.
var typeChanges = []typeChangeCase{
	{name: "text to uuid", from: "text", to: "uuid", goTy: "uuid.UUID"},
	{name: "uuid to text", from: "uuid", to: "text", goTy: "string"},
	{name: "bytea to uuid", from: "bytea", to: "uuid", goTy: "uuid.UUID"},
	{name: "uuid to bigint", from: "uuid", to: "int8", goTy: "int64"},
}

// convertible is the declaration the staged project starts and ends with. The
// column under test is the only thing that moves.
func convertible(pgtype, goTy string) string {
	imports := "import \"github.com/google/uuid\"\n"
	if !strings.Contains(goTy, "uuid.") {
		imports = ""
	}
	return "package convert\n\n" + imports + `
//orm:table public.convertibles
type Convertible struct {
	ID    ` + "int32 `orm:\"pk\"`" + `
	Value ` + goTy + " `orm:\"pgtype:" + pgtype + "\"`" + `
}
`
}

// The matrix. Each case gets its own database and its own staged project.
//
// The outcome is not the one the question anticipated, and it is recorded as it
// is. The planner plans all four: it compares two declared types, sees they
// differ, and writes an ALTER. The refusal comes from PostgreSQL at apply time,
// because it will not cast text, bytea or uuid to an unrelated type without
// being told how — and the ORM does not tell it, which is the part that matters.
// An invented USING expression would be the tool deciding, on the operator's
// behalf, what to do with the rows that do not convert.
//
// So what is asserted is the safety rather than the stage. Either the migration
// applies and the catalog agrees, or it fails and leaves nothing behind: the
// transaction rolls back, the column keeps its old type, the migration is not
// recorded, and running it again fails the same way.
func TestUUID_migrationTypeChangeSafety(t *testing.T) {
	dsn(t)

	for _, c := range typeChanges {
		t.Run(c.name, func(t *testing.T) {
			s := stage(t, "uuidconv_"+strings.ReplaceAll(c.name, " ", "_"))
			pkg := filepath.Join(s.dir, "convert")
			if err := os.MkdirAll(pkg, 0o755); err != nil {
				t.Fatal(err)
			}
			s.addPackage("./convert")

			startGo := "string"
			switch c.from {
			case "uuid":
				startGo = "uuid.UUID"
			case "bytea":
				startGo = "[]byte"
			}
			write(t, filepath.Join(pkg, "entities.go"), convertible(c.from, startGo))
			s.mustORM("makemigrations")
			s.mustORM("migrate")
			if got := s.columnType("public.convertibles", "value"); !isType(got, c.from) {
				t.Fatalf("the column starts as %s, want %s", got, c.from)
			}
			applied := s.appliedCount()

			// Move it.
			write(t, filepath.Join(pkg, "entities.go"), convertible(c.to, c.goTy))
			planOut, planErr := s.orm("makemigrations")
			if planErr != nil {
				// A planner refusal is also a correct answer, and it must leave
				// no artifact behind.
				if n := len(migrationFiles(t, s.dir)); n != applied {
					t.Errorf("the planner refused and still wrote an artifact (%d files, "+
						"%d applied)", n, applied)
				}
				t.Logf("REFUSED BY THE PLANNER %s -> %s:\n%s", c.from, c.to, planOut)
				return
			}
			if len(migrationFiles(t, s.dir)) == applied {
				t.Fatalf("makemigrations succeeded and wrote nothing:\n%s", planOut)
			}

			out, err := s.orm("migrate")
			if err == nil {
				// Applied. The catalog has to agree, and the project converges.
				got := s.columnType("public.convertibles", "value")
				if !isType(got, c.to) {
					t.Errorf("after the migration the column is %s, want %s", got, c.to)
				}
				s.mustORM("check")
				t.Logf("PLANNED AND APPLIED %s -> %s", c.from, c.to)
				return
			}

			// Refused by PostgreSQL. Nothing may have changed.
			t.Logf("PLANNED, REFUSED BY POSTGRESQL %s -> %s:\n%s", c.from, c.to, out)

			if got := s.columnType("public.convertibles", "value"); !isType(got, c.from) {
				t.Errorf("after the failed migration the column is %s, want it still %s: "+
					"the transaction did not roll back cleanly", got, c.from)
			}
			if now := s.appliedCount(); now != applied {
				t.Errorf("%d migrations are recorded as applied, want %d: a migration "+
					"that failed must not be recorded", now, applied)
			}
			if !strings.Contains(out, "rolled back") {
				t.Errorf("the diagnostic does not say the transaction was rolled back, "+
					"so an operator cannot tell whether the database moved:\n%s", out)
			}
			if !strings.Contains(out, "42804") {
				t.Errorf("the diagnostic does not carry PostgreSQL's SQLSTATE:\n%s", out)
			}

			// The same failure twice: a diagnostic that varies between runs is
			// not one an operator can act on.
			second, err2 := s.orm("migrate")
			if err2 == nil {
				t.Error("the same migration failed and then succeeded")
			}
			if second != out {
				t.Errorf("the failure differs between runs:\n--- first\n%s\n--- second\n%s",
					out, second)
			}
		})
	}
}

// isType compares a format_type spelling against the tag spelling.
func isType(got, want string) bool {
	if got == want {
		return true
	}
	alias := map[string]string{
		"int8": "bigint", "int4": "integer", "int2": "smallint", "bytea": "bytea",
	}
	return alias[want] == got
}

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func migrationFiles(t *testing.T, dir string) []string {
	t.Helper()
	ms, err := filepath.Glob(filepath.Join(dir, "migrations", "*.json"))
	if err != nil {
		t.Fatal(err)
	}
	out := make([]string, 0, len(ms))
	for _, m := range ms {
		out = append(out, filepath.Base(m))
	}
	return out
}
