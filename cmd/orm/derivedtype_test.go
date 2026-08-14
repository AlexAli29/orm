package main

import (
	"strings"
	"testing"

	"github.com/AlexAli29/orm/internal/testdb"
)

// A relation can select a type that no table in the schema stores.
//
// count(*) is bigint in a database whose every table column is integer or text,
// and that is the whole shape of the defect this guards: type discovery walked
// table columns only, so the column resolved against a map that had never been
// told about bigint, and generation stopped with "has type OID 20, which was not
// loaded". A supported scalar was refused for appearing in the wrong kind of
// relation.
//
// Nothing here declares the type. It comes out of the catalog, which is the
// point — database-first is where a derived type arrives without a declaration
// to announce it.
const derivedDDL = `
CREATE TABLE public.sales (
    id     integer PRIMARY KEY,
    region text NOT NULL,
    qty    integer NOT NULL
);

CREATE VIEW public.sales_by_region AS
    SELECT region, count(*) AS orders FROM public.sales GROUP BY region;

CREATE MATERIALIZED VIEW public.sales_totals AS
    SELECT region, count(*) AS orders
    FROM public.sales GROUP BY region;

CREATE UNIQUE INDEX sales_totals_region_key ON public.sales_totals (region);
`

const derivedEntities = `package domain

//orm:table public.sales
type Sale struct {
	ID     int32 ` + "`orm:\"pk\"`" + `
	Region string
	Qty    int32
}

//orm:view public.sales_by_region
type RegionCount struct {
	Region string
	Orders int64
}

//orm:materialized-view public.sales_totals
type TotalRow struct {
	Region string
	Orders int64
}
`

func derivedProject(t *testing.T) *project {
	t.Helper()
	p := newProject(t, derivedEntities)
	p.DSN = testdb.Create(t, derivedDDL)
	writeFile(t, p.Conf, "version: 1\n\nschema:\n  dsn: \""+p.DSN+"\"\n"+
		"  search_path:\n    - public\n\npackages:\n  - path: ./domain\n    output: same\n")
	return p
}

// The fixture only proves anything while the derived types are absent from every
// table, so that is asserted rather than assumed.
func TestMutationFixturePrecondition_derivedTypes(t *testing.T) {
	if strings.Contains(derivedDDL, "bigint") || strings.Contains(derivedDDL, "numeric") {
		t.Fatal("a table in the fixture now stores a type the relations derive, " +
			"so the type would be discovered through the table and the case is lost")
	}
}

// Generation succeeds and the aggregate columns are the Go types their
// PostgreSQL types map to.
func TestDerivedType_aggregateColumnsGenerate(t *testing.T) {
	p := derivedProject(t)
	p.MustRun("generate")
	p.MustRun("check", "--generated")

	gen := readFile(t, p.Dir+"/domain/orm_tables.gen.go")
	for _, want := range []string{
		"RegionCount, int64", // count(*) on a view
		"TotalRow, int64",    // count(*) on a materialized view
	} {
		if !strings.Contains(gen, want) {
			t.Errorf("the generated code does not contain %q; a derived built-in type "+
				"was not resolved:\n%s", want, gen)
		}
	}
	if strings.Contains(gen, "interface{}") || strings.Contains(gen, ", any]") {
		t.Error("a column degraded to an untyped placeholder rather than resolving " +
			"or being refused")
	}
}

// Discovering a type is not mapping it.
//
// Widening the scope of type discovery could have made an unmapped type reachable
// and so quietly generated something for it. avg(integer) is numeric, numeric has
// no lossless built-in Go type, and the answer is still a refusal that names the
// type and the fix — not a placeholder.
const derivedUnmappedDDL = `
CREATE TABLE public.sales (
    id  integer PRIMARY KEY,
    qty integer NOT NULL
);

CREATE MATERIALIZED VIEW public.sales_mean AS
    SELECT id, avg(qty) AS mean FROM public.sales GROUP BY id;
`

const derivedUnmappedEntities = `package domain

//orm:table public.sales
type Sale struct {
	ID  int32 ` + "`orm:\"pk\"`" + `
	Qty int32
}

//orm:materialized-view public.sales_mean
type MeanRow struct {
	ID   int32
	Mean string
}
`

func TestDerivedType_unmappedDerivedTypeIsStillRefused(t *testing.T) {
	p := newProject(t, derivedUnmappedEntities)
	p.DSN = testdb.Create(t, derivedUnmappedDDL)
	writeFile(t, p.Conf, "version: 1\n\nschema:\n  dsn: \""+p.DSN+"\"\n"+
		"  search_path:\n    - public\n\npackages:\n  - path: ./domain\n    output: same\n")

	code, stdout, stderr := p.Run("generate")
	if code == 0 {
		t.Fatal("generation succeeded for a column typed numeric; an unmapped type " +
			"must be refused rather than guessed at")
	}
	out := stdout + stderr
	if !strings.Contains(out, "E013") || !strings.Contains(out, "numeric") {
		t.Errorf("the refusal does not name the unmapped type:\n%s", out)
	}
}
