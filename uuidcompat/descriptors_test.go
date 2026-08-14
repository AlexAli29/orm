package uuidcompat_test

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// What the generator writes for a uuid column.
//
// The other files here run the generated code, which is the right way to prove
// behaviour and the wrong way to localise a fault: a descriptor with the wrong
// capability does not fail, it fails to compile, and a package that does not
// compile tells you nothing about which decision was wrong.
//
// So this reads the generated text. Each property is its own subtest, because
// each is a separate decision the generator makes about uuid and each can be
// wrong on its own.

var (
	descOnce sync.Once
	descText string
)

// generatedFor stages the module, generates, and returns the descriptors.
//
// It is done once for the whole file: generation is deterministic, so nine
// subtests reading nine properties out of one generation are reading the same
// answer they would each get on their own.
func generatedFor(t *testing.T) string {
	t.Helper()
	dsn(t)
	descOnce.Do(func() {
		s := stage(t, "uuiddescriptors")
		s.mustORM("makemigrations")
		s.mustORM("migrate")
		s.mustORM("generate")
		descText = s.read(filepath.Join("domain", "orm_tables.gen.go"))
	})
	if descText == "" {
		t.Fatal("no descriptors were generated")
	}
	return descText
}

func TestUUID_generatedDescriptorShapes(t *testing.T) {
	gen := generatedFor(t)

	cases := []struct {
		name string
		want string
		why  string
	}{
		{
			name: "ordered",
			want: "ID         orm.OrdCol[User, uuid.UUID]",
			why: "PostgreSQL orders uuid, so the column carries the ordered " +
				"capability and offers Asc, Between and the magnitude comparisons",
		},
		{
			name: "nullable",
			want: "OptionalID orm.NullOrdCol[User, uuid.UUID]",
			why: "a nullable uuid keeps the ordered capability and gains IsNull; " +
				"emitting the non-nullable descriptor would let a NULL scan into a " +
				"uuid.UUID that cannot hold one",
		},
		{
			name: "array",
			want: "Tags       orm.Col[User, []uuid.UUID]",
			why: "uuid[] is a slice of the element's Go type, and an array that " +
				"resolved to its element would scan one value where there are many",
		},
		{
			name: "foreign key",
			want: "UserID     orm.OrdCol[Order, uuid.UUID]",
			why:  "a uuid foreign key is a uuid column and nothing about it is a surrogate",
		},
		{
			name: "view",
			want: "UserID  orm.NullOrdCol[UserOrder, uuid.UUID]",
			why: "a view republishes the type; its nullability is not provable, " +
				"which is why the descriptor is the nullable one",
		},
		{
			name: "materialized view",
			want: "UserID orm.NullOrdCol[UserSummary, uuid.UUID]",
			why:  "and so does a materialized view",
		},
		{
			name: "domain",
			want: "TenantID orm.OrdCol[Token, uuid.UUID]",
			why: "a domain over uuid resolves to what it is built on, so the " +
				"configured mapping reaches it without an entry of its own",
		},
		{
			name: "derived bigint",
			want: "Orders orm.NullOrdCol[UserSummary, int64]",
			why: "count(*) is bigint whether or not a table stores one; this is the " +
				"column whose type discovery had to be widened to find",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if !strings.Contains(gen, c.want) {
				t.Errorf("the generated descriptors do not contain\n\t%s\n%s", c.want, c.why)
			}
		})
	}
}

// The fixture contains what each mutation class attacks.
//
// A mutation whose fixture lacks the attacked feature cannot be caught and must
// not be recorded as a survivor: the test was never able to notice. So each
// precondition is asserted here, on clean code, before any mutation is applied.
func TestUUIDMutationPrecondition(t *testing.T) {
	gen := generatedFor(t)
	entities := readText(t, filepath.Join("domain", "entities.go"))
	conf := readText(t, "orm.yaml")

	// U01, U04: the configured mapping is the only thing that can produce
	// uuid.UUID, and the managed declarations name the PostgreSQL type.
	t.Run("U01", func(t *testing.T) {
		if !strings.Contains(conf, "github.com/google/uuid.UUID") {
			t.Fatal("orm.yaml no longer configures the uuid mapping")
		}
	})
	t.Run("U04", func(t *testing.T) {
		if !strings.Contains(entities, `orm:"pk,pgtype:uuid"`) {
			t.Fatal("no declaration carries a pgtype tag, so ignoring the tag would " +
				"change nothing here")
		}
	})

	// U02, U03: an ordered descriptor and a nullable one both exist.
	t.Run("U02", func(t *testing.T) { needs(t, gen, "orm.OrdCol[User, uuid.UUID]") })
	t.Run("U03", func(t *testing.T) { needs(t, gen, "orm.NullOrdCol[User, uuid.UUID]") })

	// U05: a column typed as a domain over uuid.
	t.Run("U05", func(t *testing.T) {
		if !strings.Contains(entities, "pgtype:public.tenant_uuid") {
			t.Fatal("no column is typed as a domain, so domain resolution cannot be " +
				"observed")
		}
	})

	// U06, U07, U08: a materialized view with a unique index over one plain
	// uuid column. A multi-column or partial index would not exercise the same
	// rule.
	t.Run("U06", func(t *testing.T) { needsIndex(t, entities) })
	t.Run("U07", func(t *testing.T) { needsIndex(t, entities) })
	t.Run("U08", func(t *testing.T) { needsIndex(t, entities) })

	// U09: an outer join over a NOT NULL uuid exists in the suite.
	t.Run("U09", func(t *testing.T) {
		src := readText(t, "nullability_test.go")
		if !strings.Contains(src, "LeftJoin(domain.Orders.Source()") {
			t.Fatal("nothing outer-joins a source whose uuid column is NOT NULL")
		}
	})

	// U10: a uuid array.
	t.Run("U10", func(t *testing.T) { needs(t, gen, "orm.Col[User, []uuid.UUID]") })

	// U11: the lock records column types, so dropping them from it is
	// observable.
	t.Run("U11", func(t *testing.T) {
		if readText(t, "orm.lock") == "" {
			t.Fatal("there is no lock to fingerprint")
		}
	})
}

func needs(t *testing.T, gen, want string) {
	t.Helper()
	if !strings.Contains(gen, want) {
		t.Fatalf("the fixture generates no %s, so a mutation of the rule that "+
			"produces it could not be noticed", want)
	}
}

func needsIndex(t *testing.T, entities string) {
	t.Helper()
	const want = "//orm:index user_summaries_key (UserID) unique"
	if !strings.Contains(entities, want) {
		t.Fatalf("the materialized view no longer declares %s; concurrent-refresh "+
			"eligibility cannot be observed", want)
	}
}

func readText(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	return string(b)
}

// The generated repository names the index a concurrent refresh may use.
//
// Eligibility is decided at generation time and written down; this is where it
// is written. A rule that stopped accepting a single-column unique index would
// leave this empty, and the only symptom at runtime is a refusal that looks
// exactly like the schema genuinely not qualifying.
func TestUUID_generatedRefreshDescriptor(t *testing.T) {
	dsn(t)
	s := stage(t, "uuidrefreshdesc")
	s.mustORM("makemigrations")
	s.mustORM("migrate")
	s.mustORM("generate")

	if got := s.descriptorIndex(); got != "user_summaries_key" {
		t.Errorf("the generated descriptor names %q as the concurrent-refresh index, "+
			"want user_summaries_key: the unique index over a single uuid column "+
			"qualifies, and a descriptor that says otherwise refuses every "+
			"concurrent refresh for good", got)
	}
}

// Removing the qualifying index moves the lock.
//
// The lock is what check --generated compares against, so a change in what a
// concurrent refresh may do has to reach it. If it did not, generated code that
// still believes in a deleted index would be reported as current.
func TestUUID_eligibilityChangeMovesTheLock(t *testing.T) {
	dsn(t)
	s := stage(t, "uuidrefreshlock")
	s.mustORM("makemigrations")
	s.mustORM("migrate")
	s.mustORM("generate")
	before := s.read("orm.lock")

	s.declareIndex(false)
	s.mustORM("makemigrations")
	s.mustORM("migrate")
	s.mustORM("generate")

	if after := s.read("orm.lock"); after == before {
		t.Error("orm.lock is unchanged after the materialized view lost the index a " +
			"concurrent refresh needs; the fingerprint does not record what the " +
			"generator decided, so stale generated code would be called current")
	}
}

// With no qualifying index, a concurrent refresh is refused before any SQL.
//
// This is the half of the contract that costs nothing to get wrong: asking
// PostgreSQL and letting it refuse produces an error too, and only the statement
// count tells the two apart.
func TestUUID_refreshRefusesLocallyWithNoQualifyingIndex(t *testing.T) {
	dsn(t)
	s := stage(t, "uuidrefuselocal")
	s.declareIndex(false)
	s.mustORM("makemigrations")
	s.mustORM("migrate")
	s.mustORM("generate")

	got := s.refresh()
	if got.Err == "" {
		t.Fatal("a concurrent refresh succeeded with no unique index anywhere")
	}
	if got.Statements != 0 {
		t.Errorf("the refusal sent %d statements, want 0", got.Statements)
	}
	if got.IsPgError {
		t.Errorf("the refusal came from PostgreSQL (%s); it should not have got there",
			got.SQLState)
	}
}

// A column type that changed makes check --generated say so.
//
// The lock is a digest, so this is the behaviour the digest exists for: if
// column types left the fingerprint, generated code describing a uuid column
// that is now text would be reported as current.
func TestUUID_typeChangeIsCaughtByCheckGenerated(t *testing.T) {
	dsn(t)
	s := stage(t, "uuidtypecheck")
	s.mustORM("makemigrations")
	s.mustORM("migrate")
	s.mustORM("generate")
	s.mustORM("check", "--generated")

	src := s.read(filepath.Join("domain", "entities.go"))
	changed := strings.Replace(src,
		"TenantID uuid.UUID `orm:\"pgtype:public.tenant_uuid\"`",
		"TenantID string    `orm:\"pgtype:text\"`", 1)
	if changed == src {
		t.Fatal("the declaration this test edits is not in the shape it expects")
	}
	if err := os.WriteFile(filepath.Join(s.dir, "domain", "entities.go"),
		[]byte(changed), 0o644); err != nil {
		t.Fatal(err)
	}
	s.mustORM("makemigrations")
	s.mustORM("migrate")

	out, err := s.orm("check", "--generated")
	if err == nil {
		t.Errorf("check --generated passed after a column changed from a uuid domain "+
			"to text without regeneration:\n%s", out)
	}
}

// Changing only the PostgreSQL type moves the lock.
//
// The type-change test above moves both halves at once — a uuid domain becomes
// text, and uuid.UUID becomes string — so it cannot tell which half the
// fingerprint noticed. This moves the column from a domain over uuid to plain
// uuid and leaves the Go type alone, which is a real schema change and a
// no-change as far as Go is concerned.
func TestUUID_changingOnlyThePostgreSQLTypeMovesTheLock(t *testing.T) {
	dsn(t)
	s := pgTypeOnlyChange(t, "uuidpgonly_lock")
	if s.after == s.before {
		t.Error("orm.lock is unchanged after a column moved from a domain over uuid " +
			"to plain uuid; the fingerprint does not record the PostgreSQL type, so " +
			"a schema change invisible to Go is invisible to review")
	}
}

// And check --generated reports the generated code as behind.
func TestUUID_postgresTypeOnlyChangeIsCaughtByCheckGenerated(t *testing.T) {
	dsn(t)
	s := pgTypeOnlyChange(t, "uuidpgonly_check")
	if s.checkErr == nil {
		t.Errorf("check --generated passed after the column's PostgreSQL type changed "+
			"without regeneration:\n%s", s.checkOut)
	}
}

type pgOnlyResult struct {
	before, after string
	checkOut      string
	checkErr      error
}

// pgTypeOnlyChange moves tokens.tenant_id from the uuid domain to plain uuid,
// which changes the schema and nothing about Go.
func pgTypeOnlyChange(t *testing.T, dbname string) pgOnlyResult {
	t.Helper()
	s := stage(t, dbname)
	s.mustORM("makemigrations")
	s.mustORM("migrate")
	s.mustORM("generate")
	s.mustORM("check", "--generated")

	src := s.read(filepath.Join("domain", "entities.go"))
	changed := strings.Replace(src, "pgtype:public.tenant_uuid", "pgtype:uuid", 1)
	if changed == src {
		t.Fatal("the domain-typed declaration is not in the shape this test edits, " +
			"so the change below would be a no-op")
	}
	if strings.Contains(changed, "TenantID string") {
		t.Fatal("this edit changed the Go type as well; the point is that it does not")
	}
	if err := os.WriteFile(filepath.Join(s.dir, "domain", "entities.go"),
		[]byte(changed), 0o644); err != nil {
		t.Fatal(err)
	}

	before := s.read("orm.lock")
	s.mustORM("makemigrations")
	s.mustORM("migrate")
	out, err := s.orm("check", "--generated")
	s.mustORM("generate")
	return pgOnlyResult{before: before, after: s.read("orm.lock"), checkOut: out, checkErr: err}
}
