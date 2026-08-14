package orm

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// Chunking is tested from inside the package because the parameter ceiling is
// not API: exporting a knob so a test could turn it down would put a tuning
// detail in everyone's documentation.

type chunkRow struct {
	A, B, C int64
}

var chunkMeta = EntityMeta[chunkRow]{
	Table:  TableID{Schema: "public", Name: "chunk"},
	Source: NewSource("public", "chunk"),
	Columns: []ColumnMeta{
		{Name: "a", Field: "A", NotNull: true},
		{Name: "b", Field: "B", NotNull: true},
		{Name: "c", Field: "C", NotNull: true},
	},
	Dest: func(r *chunkRow, idx int) any {
		switch idx {
		case 0:
			return &r.A
		case 1:
			return &r.B
		case 2:
			return &r.C
		}
		return nil
	},
	Value: func(r *chunkRow, idx int) any {
		switch idx {
		case 0:
			return r.A
		case 1:
			return r.B
		case 2:
			return r.C
		}
		return nil
	},
}

// echoExecutor answers an insert by returning the rows it was asked to write,
// so a test can check both how the work was split and that the pieces came back
// in order.
type echoExecutor struct {
	statements []string
	rowCounts  []int
	next       int64
}

func (e *echoExecutor) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	e.statements = append(e.statements, sql)
	n := len(args) / 3
	e.rowCounts = append(e.rowCounts, n)

	rows := make([][]any, 0, n)
	for i := range n {
		e.next++
		rows = append(rows, []any{e.next, args[i*3+1], args[i*3+2]})
	}
	return &echoRows{rows: rows}, nil
}

type echoRows struct {
	rows [][]any
	i    int
}

func (r *echoRows) Next() bool {
	if r.i >= len(r.rows) {
		return false
	}
	r.i++
	return true
}

func (r *echoRows) Scan(dest ...any) error {
	row := r.rows[r.i-1]
	if len(row) != len(dest) {
		return fmt.Errorf("%d values for %d destinations", len(row), len(dest))
	}
	for i, v := range row {
		p, ok := dest[i].(*int64)
		if !ok {
			return fmt.Errorf("destination %d is %T", i, dest[i])
		}
		n, ok := v.(int64)
		if !ok {
			return fmt.Errorf("value %d is %T", i, v)
		}
		*p = n
	}
	return nil
}

func (r *echoRows) Close()                                       {}
func (r *echoRows) Err() error                                   { return nil }
func (r *echoRows) CommandTag() pgconn.CommandTag                { return pgconn.NewCommandTag("INSERT 0 1") }
func (r *echoRows) FieldDescriptions() []pgconn.FieldDescription { return nil }
func (r *echoRows) Values() ([]any, error)                       { return nil, nil }
func (r *echoRows) RawValues() [][]byte                          { return nil }
func (r *echoRows) Conn() *pgx.Conn                              { return nil }

func TestInsertMany_chunking(t *testing.T) {
	// Three columns per row, so a ceiling of six parameters is two rows per
	// statement.
	prev := maxInsertParams
	maxInsertParams = 6
	t.Cleanup(func() { maxInsertParams = prev })

	rows := make([]chunkRow, 5)
	for i := range rows {
		rows[i] = chunkRow{A: int64(i), B: int64(100 + i), C: int64(200 + i)}
	}

	ex := &echoExecutor{}
	out, err := NewRepo(ex, &chunkMeta).InsertMany(context.Background(), rows)
	if err != nil {
		t.Fatalf("InsertMany: %v", err)
	}

	if len(ex.statements) != 3 {
		t.Fatalf("ran %d statements, want 3 for five rows at two per statement", len(ex.statements))
	}
	if want := []int{2, 2, 1}; !equal(ex.rowCounts, want) {
		t.Errorf("split the rows as %v, want %v", ex.rowCounts, want)
	}

	// Every row comes back, in the order it was given.
	if len(out) != len(rows) {
		t.Fatalf("returned %d rows, want %d", len(out), len(rows))
	}
	for i, r := range out {
		if r.B != int64(100+i) || r.C != int64(200+i) {
			t.Errorf("row %d came back as %+v, out of order", i, r)
		}
		if r.A != int64(i+1) {
			t.Errorf("row %d has generated key %d, want %d", i, r.A, i+1)
		}
	}

	// The last chunk is a shorter statement, not a padded one.
	if strings.Count(ex.statements[2], "(") != strings.Count(ex.statements[0], "(")-1 {
		t.Errorf("the final chunk has the wrong shape:\n%s", ex.statements[2])
	}
}

func TestInsertMany_oneRowTooWide(t *testing.T) {
	prev := maxInsertParams
	maxInsertParams = 2
	t.Cleanup(func() { maxInsertParams = prev })

	ex := &echoExecutor{}
	_, err := NewRepo(ex, &chunkMeta).InsertMany(context.Background(), []chunkRow{{}})
	if err == nil {
		t.Fatal("InsertMany accepted a row wider than one statement may carry")
	}
	if !strings.Contains(err.Error(), "more than the 2 one statement may carry") {
		t.Errorf("error = %v", err)
	}
	if len(ex.statements) != 0 {
		t.Errorf("ran %d statements", len(ex.statements))
	}
}

func TestInsert_noWritableColumns(t *testing.T) {
	// A table whose every column is generated has nothing an insert can say.
	meta := EntityMeta[chunkRow]{
		Table:   TableID{Schema: "public", Name: "chunk"},
		Source:  NewSource("public", "chunk"),
		Columns: []ColumnMeta{{Name: "a", Field: "A", NotNull: true, Generated: true}},
		Dest:    func(r *chunkRow, idx int) any { return &r.A },
		Value:   func(r *chunkRow, idx int) any { return r.A },
	}
	ex := &echoExecutor{}
	if _, err := NewRepo(ex, &meta).Insert(context.Background(), chunkRow{}); err == nil {
		t.Fatal("Insert accepted a table with no writable column")
	}
	if len(ex.statements) != 0 {
		t.Errorf("ran %d statements", len(ex.statements))
	}
}

func TestInsert_metadataWithoutAValueAccessor(t *testing.T) {
	meta := EntityMeta[chunkRow]{
		Table:   TableID{Schema: "public", Name: "chunk"},
		Source:  NewSource("public", "chunk"),
		Columns: []ColumnMeta{{Name: "a", Field: "A", NotNull: true}},
		Dest:    func(r *chunkRow, idx int) any { return &r.A },
	}
	_, err := NewRepo(&echoExecutor{}, &meta).Insert(context.Background(), chunkRow{})
	if err == nil {
		t.Fatal("Insert succeeded with metadata that cannot read the entity")
	}
	if !strings.Contains(err.Error(), "no value accessor") {
		t.Errorf("error = %v", err)
	}
}

func equal(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
