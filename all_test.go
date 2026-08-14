package orm_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/AlexAli29/orm"
	"github.com/jackc/pgx/v5"
)

func ptr[T any](v T) *T { return &v }

// row builds one stub row in the column order userMeta declares.
func row(id int64, email string, age int32, active bool, nickname *string, deletedAt *time.Time, createdAt time.Time) []any {
	var nick, del any
	if nickname != nil {
		nick = *nickname
	}
	if deletedAt != nil {
		del = *deletedAt
	}
	// manager_id is always NULL here; the tests that care about it compare
	// columns rather than read values.
	return []any{id, email, age, active, nick, nil, del, createdAt}
}

func TestAll_scansEveryRowIntoTheEntity(t *testing.T) {
	closed := 0
	ex := stubExecutor{
		closed: &closed,
		rows: [][]any{
			row(1, "a@example.com", 30, true, ptr("alex"), nil, epoch),
			row(2, "b@example.com", 40, false, nil, ptr(epoch), epoch.Add(time.Hour)),
		},
	}

	users, err := orm.NewRepo(ex, &userMeta).Query().All(t.Context())
	if err != nil {
		t.Fatalf("All: %v", err)
	}
	if len(users) != 2 {
		t.Fatalf("got %d users, want 2", len(users))
	}

	first := users[0]
	if first.ID != 1 || first.Email != "a@example.com" || first.Age != 30 || !first.Active {
		t.Errorf("first row = %+v", first)
	}
	if first.Nickname == nil || *first.Nickname != "alex" {
		t.Errorf("first.Nickname = %v, want alex", first.Nickname)
	}
	if first.DeletedAt != nil {
		t.Errorf("first.DeletedAt = %v, want nil", first.DeletedAt)
	}

	second := users[1]
	if second.Nickname != nil {
		t.Errorf("second.Nickname = %v, want nil", second.Nickname)
	}
	if second.DeletedAt == nil || !second.DeletedAt.Equal(epoch) {
		t.Errorf("second.DeletedAt = %v", second.DeletedAt)
	}

	if closed != 1 {
		t.Errorf("rows closed %d times, want exactly 1", closed)
	}
}

func TestAll_emptyResult(t *testing.T) {
	users, err := orm.NewRepo(stubExecutor{}, &userMeta).Query().All(t.Context())
	if err != nil {
		t.Fatalf("All: %v", err)
	}
	if len(users) != 0 {
		t.Errorf("got %d users, want none", len(users))
	}
}

func TestAll_passesTheCompiledStatementToTheExecutor(t *testing.T) {
	var seen recorder
	_, err := orm.NewRepo(&seen, &userMeta).Query().
		Where(Users.Active.Eq(true), Users.Age.Gte(int32(18))).
		Limit(5).
		All(t.Context())
	if err != nil {
		t.Fatalf("All: %v", err)
	}
	want := selectAll + ` WHERE "users"."active" = $1 AND "users"."age" >= $2 LIMIT 5`
	if seen.sql != want {
		t.Errorf("executed\n%s\nwant\n%s", seen.sql, want)
	}
	assertArgs(t, seen.args, []any{true, int32(18)})
}

func TestAll_doesNotTouchTheDatabaseWhenTheQueryIsInvalid(t *testing.T) {
	var seen recorder
	_, err := orm.NewRepo(&seen, &userMeta).Query().Limit(-3).All(t.Context())
	if err == nil {
		t.Fatal("All succeeded with a negative limit")
	}
	if seen.calls != 0 {
		t.Errorf("the executor was called %d times for a query that never compiled", seen.calls)
	}
}

func TestAll_errorPaths(t *testing.T) {
	sentinel := errors.New("boom")
	tests := []struct {
		name string
		ex   stubExecutor
		want string
	}{
		{
			name: "query fails",
			ex:   stubExecutor{queryErr: sentinel},
			want: "querying public.users",
		},
		{
			name: "scan fails",
			ex:   stubExecutor{rows: [][]any{row(1, "a", 1, true, nil, nil, epoch)}, scanErr: sentinel},
			want: "scanning public.users",
		},
		{
			name: "the stream fails part way",
			ex:   stubExecutor{rows: [][]any{row(1, "a", 1, true, nil, nil, epoch)}, rowsErr: sentinel},
			want: "reading public.users",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := orm.NewRepo(tt.ex, &userMeta).Query().All(t.Context())
			if err == nil {
				t.Fatal("All succeeded")
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("error = %v, want it to contain %q", err, tt.want)
			}
			// The cause must survive wrapping, or a caller cannot tell a
			// cancelled context from a broken query.
			if !errors.Is(err, sentinel) {
				t.Errorf("error = %v, want it to wrap the cause", err)
			}
		})
	}
}

func TestAll_rowsAreClosedEvenWhenScanningFails(t *testing.T) {
	closed := 0
	ex := stubExecutor{
		closed:  &closed,
		rows:    [][]any{row(1, "a", 1, true, nil, nil, epoch)},
		scanErr: errors.New("boom"),
	}
	if _, err := orm.NewRepo(ex, &userMeta).Query().All(t.Context()); err == nil {
		t.Fatal("All succeeded")
	}
	if closed != 1 {
		t.Errorf("rows closed %d times after a scan failure, want 1", closed)
	}
}

func TestAll_metadataWithoutADestinationForEveryColumn(t *testing.T) {
	// A generated Dest that falls through returns nil, which would otherwise
	// reach pgx as a nil scan target and fail somewhere less explicable.
	meta := orm.EntityMeta[User]{
		Table:   orm.TableID{Schema: "public", Name: "users"},
		Columns: []orm.ColumnMeta{{Name: "id", Field: "ID"}, {Name: "email", Field: "Email"}},
		Dest: func(u *User, idx int) any {
			if idx == 0 {
				return &u.ID
			}
			return nil
		},
	}
	ex := stubExecutor{rows: [][]any{{int64(1), "a"}}}
	_, err := orm.NewRepo(ex, &meta).Query().All(t.Context())
	if err == nil {
		t.Fatal("All succeeded with metadata that cannot address every column")
	}
	if !strings.Contains(err.Error(), "no destination for column 1 (email)") {
		t.Errorf("error = %v, want it to name the column it could not address", err)
	}
}

// recorder is an executor that remembers what it was asked to run, and hands
// back whatever rows the test set.
type recorder struct {
	sql   string
	args  []any
	calls int
	rows  [][]any
}

func (r *recorder) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	r.calls++
	r.sql, r.args = sql, args
	return &stubRows{rows: r.rows}, nil
}
