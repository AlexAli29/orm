package orm_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// M16.5: the capability of a generated source is its type.
//
// A view has no Insert to call. That is the whole claim, and it cannot be
// tested by calling Insert and checking for an error — the point is that the
// call does not compile. So these tests compile real programs against the real
// package and require the compiler's answer.
//
// The negative half matters more than the positive. A generated API that
// offered Insert on a view and failed at runtime would fail on the one path a
// test suite is least likely to cover: the write nobody meant to write.

// compileFixture writes a program importing the ORM and reports the compiler's
// output, empty when it built.
func compileFixture(t *testing.T, body string) string {
	t.Helper()
	root, err := filepath.Abs(".")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	writeFixture(t, filepath.Join(dir, "go.mod"),
		"module example.com/capability\n\ngo 1.24\n\nrequire github.com/AlexAli29/orm v0.0.0\n\n"+
			"replace github.com/AlexAli29/orm => "+root+"\n")
	sum, err := os.ReadFile(filepath.Join(root, "go.sum"))
	if err != nil {
		t.Fatal(err)
	}
	writeFixture(t, filepath.Join(dir, "go.sum"), string(sum))
	writeFixture(t, filepath.Join(dir, "main.go"), fixturePreamble+body+"\n")

	cmd := exec.Command("go", "build", "./...")
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GOFLAGS=-mod=mod")
	out, err := cmd.CombinedOutput()
	if err == nil {
		return ""
	}
	return string(out)
}

func writeFixture(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// fixturePreamble stands in for what generated code declares: an entity, its
// metadata, and one repository of each capability.
const fixturePreamble = `package main

import (
	"context"

	"github.com/AlexAli29/orm"
)

type Row struct {
	ID   int64
	Name string
}

var meta = orm.EntityMeta[Row]{
	Table:   orm.TableID{Schema: "public", Name: "rows"},
	Columns: []orm.ColumnMeta{{Name: "id"}, {Name: "name"}},
	Dest: func(e *Row, i int) any {
		if i == 0 {
			return &e.ID
		}
		return &e.Name
	},
	Value: func(e *Row, i int) any {
		if i == 0 {
			return e.ID
		}
		return e.Name
	},
}

var (
	ctx     = context.Background()
	table   = orm.NewRepo(nil, &meta)
	view    = orm.NewViewRepo(nil, &meta)
	matview = orm.NewMaterializedViewRepo(nil, &meta, "rows_pkey")
)

var _ = ctx

func main() {
`

// The writes that must not exist. Each is compiled on its own, because one
// program with all of them would prove only that at least one failed.
func TestCapability_writesDoNotCompileOnViews(t *testing.T) {
	for _, c := range []struct {
		what string
		body string
	}{
		{"view.Insert", "\t_, _ = view.Insert(ctx, Row{})\n}"},
		{"view.InsertMany", "\t_, _ = view.InsertMany(ctx, []Row{})\n}"},
		{"view.Update", "\t_ = view.Update()\n}"},
		{"view.Delete", "\t_ = view.Delete()\n}"},
		{"view.CopyFrom", "\t_, _ = view.CopyFrom(ctx, []Row{})\n}"},
		{"matview.Insert", "\t_, _ = matview.Insert(ctx, Row{})\n}"},
		{"matview.InsertMany", "\t_, _ = matview.InsertMany(ctx, []Row{})\n}"},
		{"matview.Update", "\t_ = matview.Update()\n}"},
		{"matview.Delete", "\t_ = matview.Delete()\n}"},
		{"matview.CopyFrom", "\t_, _ = matview.CopyFrom(ctx, []Row{})\n}"},
	} {
		t.Run(c.what, func(t *testing.T) {
			out := compileFixture(t, c.body)
			if out == "" {
				t.Fatalf("%s compiled. A write API on a read-only source is a runtime "+
					"failure waiting on the least-tested path there is", c.what)
			}
			// And it must fail because the method is absent, not because the
			// fixture is broken in some unrelated way.
			if !strings.Contains(out, "undefined") && !strings.Contains(out, "has no field or method") {
				t.Errorf("%s failed to compile for the wrong reason:\n%s", c.what, out)
			}
		})
	}
}

// And the reads that must exist, including the ones a materialized view gets by
// being a view. A test that only proved absence would be satisfied by a type
// with no methods at all.
func TestCapability_readsCompileOnViews(t *testing.T) {
	for _, c := range []struct {
		what string
		body string
	}{
		{"view.Query().All", "\t_, _ = view.Query().All(ctx)\n}"},
		{"view.Query().Where", "\t_, _ = view.Query().Where().All(ctx)\n}"},
		{"view.QueryFrom", "\t_, _ = view.QueryFrom(nil).All(ctx)\n}"},
		{"view.Query().SQL", "\t_, _, _ = view.Query().SQL()\n}"},
		{"matview.Query().All", "\t_, _ = matview.Query().All(ctx)\n}"},
		{"matview.QueryFrom", "\t_, _ = matview.QueryFrom(nil).All(ctx)\n}"},
		{"matview.Refresh", "\t_ = matview.Refresh(ctx)\n}"},
		{"matview.Refresh(Concurrently)", "\t_ = matview.Refresh(ctx, orm.Concurrently())\n}"},
		{"matview.Refresh(WithNoData)", "\t_ = matview.Refresh(ctx, orm.WithNoData())\n}"},
		{"table keeps its writes", "\t_, _ = table.Insert(ctx, Row{})\n\t_, _ = table.CopyFrom(ctx, []Row{})\n}"},
	} {
		t.Run(c.what, func(t *testing.T) {
			if out := compileFixture(t, c.body); out != "" {
				t.Errorf("%s does not compile:\n%s", c.what, out)
			}
		})
	}
}

// Refresh is not on an ordinary view: PostgreSQL has nothing to refresh there.
func TestCapability_refreshIsNotOnAnOrdinaryView(t *testing.T) {
	if out := compileFixture(t, "\t_ = view.Refresh(ctx)\n}"); out == "" {
		t.Error("an ordinary view has a Refresh method; PostgreSQL has no such statement for one")
	}
}
