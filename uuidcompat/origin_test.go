package uuidcompat_test

import (
	"testing"

	"example.com/uuidcompat/domain"
	"github.com/AlexAli29/orm"
	"github.com/google/uuid"
)

// Where a uuid comes from, as opposed to what it is.
//
// Two boundaries meet in public.tokens. One column is typed as a domain over
// uuid; another is left to the server to fill in. Both are places where a
// configured mapping could stop applying — a domain is a distinct PostgreSQL
// type with its own name, and a value the application never supplied is one the
// Go side never saw.

// A domain over uuid resolves to the type it is built on, and the configured
// uuid mapping serves it without an entry of its own.
func TestUUID_domainOverUUIDResolvesToTheBaseType(t *testing.T) {
	pool, _ := open(t)

	var typname, basename string
	err := pool.QueryRow(t.Context(), `
		SELECT t.typname, b.typname
		  FROM pg_attribute a
		  JOIN pg_type t ON t.oid = a.atttypid
		  LEFT JOIN pg_type b ON b.oid = t.typbasetype
		 WHERE a.attrelid = 'public.tokens'::regclass AND a.attname = 'tenant_id'`).
		Scan(&typname, &basename)
	if err != nil {
		t.Fatalf("reading tokens.tenant_id: %v", err)
	}
	if typname != "tenant_uuid" {
		t.Errorf("tokens.tenant_id is %s, want the domain tenant_uuid: if the column "+
			"were plain uuid this would prove nothing about domains", typname)
	}
	if basename != "uuid" {
		t.Errorf("the domain is built on %s, want uuid", basename)
	}
	// And in Go it is the ordinary mapped type. A domain that fell out of the
	// mapping would have been refused at generation, not silently widened, but
	// this states the answer rather than the absence of a complaint.
	var _ uuid.UUID = domain.Token{}.TenantID
}

// Values written through the domain column come back as themselves.
func TestUUID_domainColumnRoundTrips(t *testing.T) {
	pool, db := open(t)
	if _, err := pool.Exec(t.Context(), `TRUNCATE tokens`); err != nil {
		t.Fatalf("clearing: %v", err)
	}

	id, tenant := uuid.New(), uuid.New()
	if _, err := db.Tokens.Insert(t.Context(), domain.Token{
		ID: id, TenantID: tenant, Value: uuid.New(),
	}); err != nil {
		t.Fatalf("inserting: %v", err)
	}

	got, err := db.Tokens.Query().Where(domain.Tokens.TenantID.Eq(tenant)).One(t.Context())
	if err != nil {
		t.Fatalf("querying by the domain column: %v", err)
	}
	if got.ID != id || got.TenantID != tenant {
		t.Errorf("read %v/%v, want %v/%v", got.ID, got.TenantID, id, tenant)
	}
}

// The server supplies the value when it is asked to, and the value is a uuid.
//
// gen_random_uuid() is core PostgreSQL from 13, so it is present on every major
// this project supports and needs no extension. That is the extent of the
// claim: this function on these majors, not that some UUID-generating function
// is available everywhere. The portable contract is the other one — that an
// application may always supply the value itself, which every other uuid column
// in this module does.
func TestUUID_serverGeneratedDefault(t *testing.T) {
	pool, db := open(t)
	if _, err := pool.Exec(t.Context(), `TRUNCATE tokens`); err != nil {
		t.Fatalf("clearing: %v", err)
	}

	id := uuid.New()
	// Value is deliberately left at its Go zero, and Default is what says so:
	// without it the zero UUID would be stored, because a zero value is a value.
	written, err := db.Tokens.Insert(t.Context(),
		domain.Token{ID: id, TenantID: uuid.New()},
		orm.Default(domain.Tokens.Value))
	if err != nil {
		t.Fatalf("inserting with a server default: %v", err)
	}
	if written.Value == uuid.Nil {
		t.Fatal("the server default produced the zero UUID; either the column was " +
			"written with the Go zero value or the default did not apply")
	}

	got, err := db.Tokens.Query().Where(domain.Tokens.ID.Eq(id)).One(t.Context())
	if err != nil {
		t.Fatalf("reading back: %v", err)
	}
	if got.Value != written.Value {
		t.Errorf("the returned value %v is not the stored one %v", written.Value, got.Value)
	}

	// Two rows get two different values, so this is generation rather than a
	// constant that happens not to be zero.
	second, err := db.Tokens.Insert(t.Context(),
		domain.Token{ID: uuid.New(), TenantID: uuid.New()},
		orm.Default(domain.Tokens.Value))
	if err != nil {
		t.Fatalf("inserting a second: %v", err)
	}
	if second.Value == written.Value {
		t.Errorf("both rows got %v; the default is not generating", second.Value)
	}
}

// Without Default, the Go zero value is stored, because a zero value is a value.
func TestUUID_zeroValueIsStoredRatherThanDefaulted(t *testing.T) {
	pool, db := open(t)
	if _, err := pool.Exec(t.Context(), `TRUNCATE tokens`); err != nil {
		t.Fatalf("clearing: %v", err)
	}

	id := uuid.New()
	written, err := db.Tokens.Insert(t.Context(), domain.Token{ID: id, TenantID: uuid.New()})
	if err != nil {
		t.Fatalf("inserting: %v", err)
	}
	if written.Value != uuid.Nil {
		t.Errorf("value is %v, want the zero UUID: a field left alone is a value and "+
			"must not silently become the column's default", written.Value)
	}
}
