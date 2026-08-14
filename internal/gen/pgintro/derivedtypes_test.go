package pgintro_test

import (
	"context"
	"testing"

	"github.com/AlexAli29/orm/internal/gen/model"
	"github.com/AlexAli29/orm/internal/gen/pgintro"
	"github.com/AlexAli29/orm/internal/testdb"
)

// A stored query can produce a type that no table in the schema stores.
//
// SELECT count(*) is bigint whether or not a bigint column exists anywhere, and
// avg(integer) is numeric for the same reason. Type discovery once walked table
// columns only, so those columns resolved against a map that had never heard of
// their OID and introspection failed with "has type OID 20, which was not
// loaded" — a supported scalar refused because of where it appeared.
//
// Not one of these tables has a bigint or numeric column. That is the whole
// point of the fixture: remove it and every case below passes for the wrong
// reason, because the type would arrive through the table instead.
const derivedTypeSchema = `
CREATE TABLE sales (
	id      integer PRIMARY KEY,
	region  text NOT NULL,
	qty     integer NOT NULL
);

-- An ordinary view, because this was never specific to materialized ones.
CREATE VIEW sales_by_region AS
	SELECT region, count(*) AS orders FROM sales GROUP BY region;

CREATE MATERIALIZED VIEW sales_totals AS
	SELECT region, count(*) AS orders, sum(qty) AS units, avg(qty) AS mean
	FROM sales GROUP BY region WITH DATA;
`

func introspectDerived(t *testing.T) *model.Schema {
	t.Helper()
	testdb.AdminDSN(t)
	dsn := testdb.Create(t, derivedTypeSchema)
	conn, err := pgintro.Connect(t.Context(), dsn)
	if err != nil {
		t.Fatalf("connecting: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close(context.Background()) })
	s, err := pgintro.Introspect(t.Context(), conn, []string{"public"})
	if err != nil {
		t.Fatalf("introspecting: %v", err)
	}
	return s
}

// The fixture is only meaningful while no table column carries the derived
// types, so it says so out loud rather than trusting the schema above to stay
// that way.
func TestIntrospect_derivedTypesAreOnNoTableColumn(t *testing.T) {
	s := introspectDerived(t)
	for _, tb := range s.Tables {
		for _, c := range tb.Cols {
			switch c.Type.Name {
			case "int8", "numeric":
				t.Fatalf("%s.%s is %s: a table now stores a type the views derive, "+
					"so this fixture no longer proves anything", tb.Name, c.Name, c.Type.Name)
			}
		}
	}
}

// count(*) on a view is bigint, and bigint is a supported scalar.
func TestIntrospect_viewAggregateResolvesToItsBuiltInType(t *testing.T) {
	v := find(t, introspectDerived(t), "sales_by_region")

	c := column(t, v, "orders")
	if c.Type == nil || c.Type.Name != "int8" {
		t.Errorf("orders has type %v, want int8: count(*) is bigint whether or not "+
			"a table column happens to be", c.Type)
	}
}

// The same on a materialized view, and for the two aggregates whose result type
// is not the type of the argument.
func TestIntrospect_materializedViewAggregatesResolveToTheirBuiltInTypes(t *testing.T) {
	v := find(t, introspectDerived(t), "sales_totals")

	want := map[string]string{
		"orders": "int8",    // count(*)
		"units":  "int8",    // sum(integer) widens to bigint
		"mean":   "numeric", // avg(integer) widens to numeric
	}
	for name, pgType := range want {
		c := column(t, v, name)
		if c.Type == nil || c.Type.Name != pgType {
			t.Errorf("%s has type %v, want %s", name, c.Type, pgType)
		}
	}
}

func column(t *testing.T, rel *model.PGTable, name string) *model.PGColumn {
	t.Helper()
	for _, c := range rel.Cols {
		if c.Name == name {
			return c
		}
	}
	t.Fatalf("%s has no column %s", rel.Name, name)
	return nil
}
