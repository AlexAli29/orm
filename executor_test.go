package orm_test

import (
	"context"
	"fmt"
	"time"

	"github.com/AlexAli29/orm"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// stubExecutor stands in for pgx so that the scan loop, the error paths and the
// resource handling can be tested without a database. The integration tests
// cover the real thing; this covers the parts that are awkward to provoke
// through one — a row that fails to scan, a stream that fails part way, and
// whether Close is actually called.
type stubExecutor struct {
	rows     [][]any
	queryErr error
	scanErr  error
	rowsErr  error

	// lastSQL and lastArgs record what the query compiled to.
	lastSQL  string
	lastArgs []any
	// closed counts Close calls, so a leak shows up as zero.
	closed *int
}

func (e stubExecutor) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	if e.queryErr != nil {
		return nil, e.queryErr
	}
	return &stubRows{rows: e.rows, scanErr: e.scanErr, rowsErr: e.rowsErr, closed: e.closed}, nil
}

type stubRows struct {
	rows    [][]any
	i       int
	scanErr error
	rowsErr error
	closed  *int
}

func (r *stubRows) Next() bool {
	if r.i >= len(r.rows) {
		return false
	}
	r.i++
	return true
}

func (r *stubRows) Scan(dest ...any) error {
	if r.scanErr != nil {
		return r.scanErr
	}
	row := r.rows[r.i-1]
	if len(row) != len(dest) {
		return fmt.Errorf("stub: %d values for %d destinations", len(row), len(dest))
	}
	for i, v := range row {
		if err := assign(dest[i], v); err != nil {
			return fmt.Errorf("stub: column %d: %w", i, err)
		}
	}
	return nil
}

func (r *stubRows) Close() {
	if r.closed != nil {
		*r.closed++
	}
}

func (r *stubRows) Err() error                                   { return r.rowsErr }
func (r *stubRows) CommandTag() pgconn.CommandTag                { return pgconn.CommandTag{} }
func (r *stubRows) FieldDescriptions() []pgconn.FieldDescription { return nil }
func (r *stubRows) Values() ([]any, error)                       { return nil, nil }
func (r *stubRows) RawValues() [][]byte                          { return nil }
func (r *stubRows) Conn() *pgx.Conn                              { return nil }

// assign copies a value into a scan destination.
//
// The type switch is deliberate: the production scan path uses no reflection,
// and a test helper that did would be testing something the real code never
// does.
func assign(dest, v any) error {
	switch d := dest.(type) {
	case *int64:
		x, ok := v.(int64)
		if !ok {
			return typeErr(v, "int64")
		}
		*d = x
	case *int32:
		x, ok := v.(int32)
		if !ok {
			return typeErr(v, "int32")
		}
		*d = x
	case *string:
		x, ok := v.(string)
		if !ok {
			return typeErr(v, "string")
		}
		*d = x
	case *bool:
		x, ok := v.(bool)
		if !ok {
			return typeErr(v, "bool")
		}
		*d = x
	case *time.Time:
		x, ok := v.(time.Time)
		if !ok {
			return typeErr(v, "time.Time")
		}
		*d = x
	case **string:
		if v == nil {
			*d = nil
			return nil
		}
		x, ok := v.(string)
		if !ok {
			return typeErr(v, "string")
		}
		*d = &x
	case **int64:
		if v == nil {
			*d = nil
			return nil
		}
		x, ok := v.(int64)
		if !ok {
			return typeErr(v, "int64")
		}
		*d = &x
	case **time.Time:
		if v == nil {
			*d = nil
			return nil
		}
		x, ok := v.(time.Time)
		if !ok {
			return typeErr(v, "time.Time")
		}
		*d = &x
	default:
		return fmt.Errorf("stub cannot assign to %T", dest)
	}
	return nil
}

func typeErr(v any, want string) error {
	return fmt.Errorf("stub has %T where %s was wanted", v, want)
}

// The Executor interface is one method so that pgx's own types satisfy it as
// they are, with no adapter to write and nothing to wrap. Generated code tells
// callers so in a comment; these assertions are what make the comment true.
//
// They are compile-time only. If pgx ever changes one of these signatures, this
// file stops building, which is the earliest and loudest place to find out.
var (
	_ orm.Executor = (*pgx.Conn)(nil)
	_ orm.Executor = (pgx.Tx)(nil)
	_ orm.Executor = (*pgxpool.Pool)(nil)
)
