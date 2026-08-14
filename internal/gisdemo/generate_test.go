package gisdemo_test

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/AlexAli29/orm/gen/diag"
	"github.com/AlexAli29/orm/internal/gen"
	"github.com/AlexAli29/orm/internal/gen/config"
	"github.com/AlexAli29/orm/internal/gen/emit"
	"github.com/AlexAli29/orm/internal/testdb"
)

// The generated spatial files are committed, so `go build ./...` compiles them
// on every run and every test in this package queries through them. That only
// means anything if the committed files are what the generator actually
// produces against a real PostGIS schema, which is what these check.

func generateHere(t *testing.T) []emit.File {
	t.Helper()
	requirePostGIS(t)
	dsn := testdb.Create(t, schemaSQL(t))
	t.Setenv("ORM_GISDEMO_DSN", dsn)

	cfg, err := config.Load("orm.yaml")
	if err != nil {
		t.Fatalf("loading orm.yaml: %v", err)
	}
	_, files, err := gen.Generate(t.Context(), cfg, diag.SeverityError)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	return files
}

func TestGenerate_matchesTheCommittedFiles(t *testing.T) {
	files := generateHere(t)
	// The relations file is not among them: these entities declare no
	// relations, and the generator writes only what a package needs.
	if len(files) == 0 || len(files) > len(emit.GeneratedFiles) {
		t.Fatalf("generated %d files", len(files))
	}
	for _, f := range files {
		name := filepath.Base(f.Path)
		committed, err := os.ReadFile(name)
		if err != nil {
			t.Fatalf("reading the committed %s: %v", name, err)
		}
		if !bytes.Equal(committed, f.Content) {
			t.Errorf("the committed %s is stale\n--- generated ---\n%s\n--- committed ---\n%s",
				name, f.Content, committed)
		}
	}
}

// Determinism matters more for spatial descriptors than for most, because three
// of their constructor arguments come from a type modifier the generator parsed
// — and a parse that went through a map would reorder without anybody noticing
// until a diff appeared on a branch nobody touched.
func TestGenerate_isByteIdenticalAcrossRuns(t *testing.T) {
	first := generateHere(t)
	for range 4 {
		again := generateHere(t)
		if len(again) != len(first) {
			t.Fatalf("a rerun produced %d files, want %d", len(again), len(first))
		}
		for i := range first {
			if first[i].Path != again[i].Path {
				t.Fatalf("file %d is %s and was %s", i, again[i].Path, first[i].Path)
			}
			if !bytes.Equal(first[i].Content, again[i].Content) {
				t.Errorf("%s differs between runs", filepath.Base(first[i].Path))
			}
		}
	}
}

// The spatial metadata has to reach the generated constructors, and the exact
// text is worth asserting: this is the one place where the SRID, the shape and
// the dimensionality stop being catalog rows and become Go.
func TestGenerate_spatialDescriptors(t *testing.T) {
	files := generateHere(t)
	var tables string
	for _, f := range files {
		if filepath.Base(f.Path) == "orm_tables.gen.go" {
			tables = string(f.Content)
		}
	}
	if tables == "" {
		t.Fatal("no orm_tables.gen.go was generated")
	}

	want := []string{
		// geometry and geography get different descriptors.
		`Spot:      postgis.NewGeogCol[Place](src, "spot", 4326, postgis.KindPoint, postgis.XY)`,
		`Location:  postgis.NewGeomCol[Place](src, "location", 4326, postgis.KindPoint, postgis.XY)`,
		// A different SRID on the same shape stays different.
		`Projected: postgis.NewNullGeomCol[Place](src, "projected", 3857, postgis.KindPoint, postgis.XY)`,
		// A nullable column gets the nullable descriptor.
		`Footprint: postgis.NewNullGeomCol[Place](src, "footprint", 4326, postgis.KindPolygon, postgis.XY)`,
		// A column with no modifier constrains nothing, and says so.
		`Sketch:    postgis.NewNullGeomCol[Place](src, "sketch", 0, postgis.AnyKind, postgis.XY)`,
		// All four dimensionalities survive.
		`Raised: postgis.NewGeomCol[Reading](src, "raised", 4326, postgis.KindPoint, postgis.XYZ)`,
		`Marked: postgis.NewGeomCol[Reading](src, "marked", 4326, postgis.KindPoint, postgis.XYM)`,
		`ZM:     postgis.NewGeomCol[Reading](src, "zm", 4326, postgis.KindPoint, postgis.XYZM)`,
		`Line3D: postgis.NewNullGeomCol[Reading](src, "line3d", 4326, postgis.KindLineString, postgis.XYZ)`,
		// And the multi shapes.
		`Area:   postgis.NewGeomCol[Zone](src, "area", 4326, postgis.KindMultiPolygon, postgis.XY)`,
		`Path: postgis.NewGeomCol[Road](src, "path", 4326, postgis.KindLineString, postgis.XY)`,
	}
	for _, w := range want {
		if !contains(tables, w) {
			t.Errorf("the generated table file does not contain:\n\t%s", w)
		}
	}

	// And the descriptor types, which are what a query is written against.
	for _, w := range []string{
		"Spot      postgis.GeogCol[Place]",
		"Location  postgis.GeomCol[Place]",
		"Projected postgis.NullGeomCol[Place]",
	} {
		if !contains(tables, w) {
			t.Errorf("the generated struct does not declare:\n\t%s", w)
		}
	}
}
