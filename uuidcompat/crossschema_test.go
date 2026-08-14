package uuidcompat_test

import (
	"strings"
	"testing"

	"example.com/uuidcompat/tenanta"
	"example.com/uuidcompat/tenantb"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Two relations that differ only by schema.
//
//	schema_a.users(id uuid primary key, label text)
//	schema_b.users(id uuid primary key, label text)
//
// Same basename, same columns, same Go type for the key. If anything in the
// generator or the runtime keyed a relation on its name — a descriptor cache, a
// migration-state entry, a source identity — these two would share it, and the
// symptom would be a query returning the other schema's rows or a migration
// that thinks one table is already there because the other is.
//
// uuid is what makes this worth its own case rather than being covered by the
// existing cross-schema tests: a configured type mapping is looked up by
// PostgreSQL type name, and there is exactly one uuid entry serving both
// schemas. A lookup that memoised per type rather than per column would still
// be correct here; a lookup that memoised per relation name would not.

func openTenants(t *testing.T) (*pgxpool.Pool, *tenanta.DB, *tenantb.DB) {
	t.Helper()
	pool, err := pgxpool.New(t.Context(), dsn(t))
	if err != nil {
		t.Fatalf("connecting: %v", err)
	}
	t.Cleanup(pool.Close)
	var n int
	if err := pool.QueryRow(t.Context(),
		`SELECT count(*) FROM pg_class c JOIN pg_namespace ns ON ns.oid = c.relnamespace
		  WHERE ns.nspname IN ('schema_a','schema_b') AND c.relname = 'users'`).Scan(&n); err != nil {
		t.Fatalf("checking the schemas: %v", err)
	}
	if n != 2 {
		t.Fatalf("found %d of the 2 cross-schema relations; the runner creates "+
			"schema_a and schema_b before migrating, because migrations do not "+
			"create schemas", n)
	}
	if _, err := pool.Exec(t.Context(), `TRUNCATE schema_a.users, schema_b.users`); err != nil {
		t.Fatalf("clearing: %v", err)
	}
	return pool, tenanta.New(pool), tenantb.New(pool)
}

// Each generated source names its own schema, and they are not the same string.
func TestUUID_crossSchemaSourcesAreDistinct(t *testing.T) {
	a := tenanta.Users.Source().String()
	b := tenantb.Users.Source().String()
	if a == b {
		t.Fatalf("both sources render as %q; two relations with the same basename "+
			"in different schemas must not share an identity", a)
	}
	if !strings.Contains(a, "schema_a") {
		t.Errorf("schema_a's source renders as %q and does not name its schema", a)
	}
	if !strings.Contains(b, "schema_b") {
		t.Errorf("schema_b's source renders as %q and does not name its schema", b)
	}
}

// Writes to one are invisible to the other, and each reads back its own uuid.
func TestUUID_crossSchemaResultsAreIndependent(t *testing.T) {
	_, da, dbb := openTenants(t)

	idA, idB := uuid.New(), uuid.New()
	if _, err := da.Users.Insert(t.Context(), tenanta.User{ID: idA, Label: "a"}); err != nil {
		t.Fatalf("inserting into schema_a: %v", err)
	}
	if _, err := dbb.Users.Insert(t.Context(), tenantb.User{ID: idB, Label: "b"}); err != nil {
		t.Fatalf("inserting into schema_b: %v", err)
	}

	as, err := da.Users.Query().All(t.Context())
	if err != nil {
		t.Fatalf("reading schema_a: %v", err)
	}
	bs, err := dbb.Users.Query().All(t.Context())
	if err != nil {
		t.Fatalf("reading schema_b: %v", err)
	}
	if len(as) != 1 || as[0].ID != idA || as[0].Label != "a" {
		t.Errorf("schema_a read %+v, want one row %v/a", as, idA)
	}
	if len(bs) != 1 || bs[0].ID != idB || bs[0].Label != "b" {
		t.Errorf("schema_b read %+v, want one row %v/b", bs, idB)
	}

	// The decisive one: each schema's key is absent from the other.
	got, err := da.Users.Query().Where(tenanta.Users.ID.Eq(idB)).All(t.Context())
	if err != nil {
		t.Fatalf("querying schema_a for schema_b's key: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("schema_a returned %d rows for a uuid only written to schema_b", len(got))
	}
}

// Both columns are uuid in PostgreSQL, and both are uuid.UUID in Go, from one
// configured mapping.
func TestUUID_crossSchemaTypesResolveFromTheOneMapping(t *testing.T) {
	pool, _, _ := openTenants(t)

	for _, s := range []string{"schema_a", "schema_b"} {
		var got string
		err := pool.QueryRow(t.Context(),
			`SELECT format_type(a.atttypid, a.atttypmod)
			   FROM pg_attribute a
			  WHERE a.attrelid = ($1||'.users')::regclass AND a.attname = 'id'`, s).Scan(&got)
		if err != nil {
			t.Fatalf("reading %s.users.id: %v", s, err)
		}
		if got != "uuid" {
			t.Errorf("%s.users.id is %s, want uuid", s, got)
		}
	}
	// Compile-time: both are uuid.UUID, and a mapping that resolved one of them
	// to something else would not build.
	var _ uuid.UUID = tenanta.User{}.ID
	var _ uuid.UUID = tenantb.User{}.ID
}

// The migration state records the two relations separately.
func TestUUID_crossSchemaMigrationStateIsPerRelation(t *testing.T) {
	pool, _, _ := openTenants(t)

	var n int
	if err := pool.QueryRow(t.Context(),
		`SELECT count(*) FROM pg_class c JOIN pg_namespace ns ON ns.oid = c.relnamespace
		  WHERE c.relname = 'users' AND ns.nspname IN ('public','schema_a','schema_b')`).Scan(&n); err != nil {
		t.Fatalf("counting: %v", err)
	}
	if n != 3 {
		t.Fatalf("found %d relations named users, want 3 (public, schema_a, schema_b): "+
			"a migration that treated the name as the identity would have created "+
			"fewer", n)
	}
}
