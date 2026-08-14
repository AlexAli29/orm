package gendemo_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/AlexAli29/orm"
	"github.com/AlexAli29/orm/internal/gendemo"
	"github.com/AlexAli29/orm/internal/testdb"
	"github.com/jackc/pgx/v5"
)

// The M3 read surface, against real PostgreSQL. The seeded data is the three
// users from gendemo_test.go: alex (30, active), sam (17, pending) and robin
// (45, banned).

func TestOne_integration(t *testing.T) {
	testdb.AdminDSN(t)
	d := db(t)

	t.Run("exactly one row", func(t *testing.T) {
		u, err := d.Users.Query().Where(gendemo.Users.Email.Eq("alex@example.com")).One(t.Context())
		if err != nil {
			t.Fatalf("One: %v", err)
		}
		if u.ID != 1 || u.Age != 30 {
			t.Errorf("One returned %+v", u)
		}
	})

	t.Run("no rows", func(t *testing.T) {
		u, err := d.Users.Query().Where(gendemo.Users.Email.Eq("nobody@example.com")).One(t.Context())
		if !errors.Is(err, orm.ErrNotFound) {
			t.Fatalf("error = %v, want ErrNotFound", err)
		}
		// User holds slices, so the zero value is compared field by field
		// through the parts that matter.
		if u.ID != 0 || u.Email != "" || u.Tags != nil {
			t.Errorf("a not-found One returned %+v", u)
		}
	})

	t.Run("more than one row", func(t *testing.T) {
		// Every seeded address ends in example.com, so this matches three.
		_, err := d.Users.Query().Where(gendemo.Users.Email.ILike("%example.com")).One(t.Context())
		if !errors.Is(err, orm.ErrMultipleRows) {
			t.Fatalf("error = %v, want ErrMultipleRows", err)
		}
	})

	t.Run("a limit of one makes the query unambiguous", func(t *testing.T) {
		// The caller asked for one row, so two can never come back and the
		// ambiguity cannot arise.
		u, err := d.Users.Query().
			Where(gendemo.Users.Email.ILike("%example.com")).
			OrderBy(gendemo.Users.ID.Asc()).
			Limit(1).
			One(t.Context())
		if err != nil {
			t.Fatalf("One: %v", err)
		}
		if u.ID != 1 {
			t.Errorf("One returned %+v", u)
		}
	})
}

func TestCount_integration(t *testing.T) {
	testdb.AdminDSN(t)
	d := db(t)

	// The contract is that Count answers "how many rows would All return".
	// Comparing the two for every combination is the only way to hold it.
	cases := []struct {
		name string
		q    func() *orm.Query[gendemo.User]
	}{
		{name: "unrestricted", q: func() *orm.Query[gendemo.User] { return d.Users.Query() }},
		{
			name: "a condition",
			q: func() *orm.Query[gendemo.User] {
				return d.Users.Query().Where(gendemo.Users.Active.Eq(true))
			},
		},
		{
			name: "a condition matching nothing",
			q: func() *orm.Query[gendemo.User] {
				return d.Users.Query().Where(gendemo.Users.Email.Eq("nobody@example.com"))
			},
		},
		{
			name: "a limit smaller than the result",
			q:    func() *orm.Query[gendemo.User] { return d.Users.Query().Limit(2) },
		},
		{
			name: "a limit larger than the result",
			q:    func() *orm.Query[gendemo.User] { return d.Users.Query().Limit(10) },
		},
		{
			name: "a limit of nothing",
			q:    func() *orm.Query[gendemo.User] { return d.Users.Query().Limit(0) },
		},
		{
			name: "an offset",
			q: func() *orm.Query[gendemo.User] {
				return d.Users.Query().OrderBy(gendemo.Users.ID.Asc()).Offset(1)
			},
		},
		{
			name: "an offset past the end",
			q: func() *orm.Query[gendemo.User] {
				return d.Users.Query().OrderBy(gendemo.Users.ID.Asc()).Offset(10)
			},
		},
		{
			name: "a limit and an offset",
			q: func() *orm.Query[gendemo.User] {
				return d.Users.Query().OrderBy(gendemo.Users.ID.Asc()).Limit(2).Offset(1)
			},
		},
		{
			name: "everything together",
			q: func() *orm.Query[gendemo.User] {
				return d.Users.Query().
					Where(gendemo.Users.Age.Gte(int32(18))).
					OrderBy(gendemo.Users.CreatedAt.Desc()).
					Limit(1).
					Offset(1)
			},
		},
	}

	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			base := tt.q()
			rows, err := base.Clone().All(t.Context())
			if err != nil {
				t.Fatalf("All: %v", err)
			}
			count, err := base.Clone().Count(t.Context())
			if err != nil {
				t.Fatalf("Count: %v", err)
			}
			if int64(len(rows)) != count {
				t.Errorf("All returned %d rows but Count said %d", len(rows), count)
			}
		})
	}
}

func TestExists_integration(t *testing.T) {
	testdb.AdminDSN(t)
	d := db(t)

	tests := []struct {
		name string
		q    func() *orm.Query[gendemo.User]
		want bool
	}{
		{
			name: "something matches",
			q: func() *orm.Query[gendemo.User] {
				return d.Users.Query().Where(gendemo.Users.Email.Eq("alex@example.com"))
			},
			want: true,
		},
		{
			name: "nothing matches",
			q: func() *orm.Query[gendemo.User] {
				return d.Users.Query().Where(gendemo.Users.Email.Eq("nobody@example.com"))
			},
		},
		{
			name: "unrestricted",
			q:    func() *orm.Query[gendemo.User] { return d.Users.Query() },
			want: true,
		},
		{
			// A query limited to no rows matches nothing, however many rows
			// the table holds.
			name: "limited to nothing",
			q:    func() *orm.Query[gendemo.User] { return d.Users.Query().Limit(0) },
		},
		{
			name: "offset past the end",
			q: func() *orm.Query[gendemo.User] {
				return d.Users.Query().OrderBy(gendemo.Users.ID.Asc()).Offset(10)
			},
		},
		{
			name: "offset within the result",
			q: func() *orm.Query[gendemo.User] {
				return d.Users.Query().OrderBy(gendemo.Users.ID.Asc()).Offset(1)
			},
			want: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := tt.q().Exists(t.Context())
			if err != nil {
				t.Fatalf("Exists: %v", err)
			}
			if got != tt.want {
				t.Errorf("Exists = %v, want %v", got, tt.want)
			}
			// Exists and Count have to agree about emptiness.
			n, err := tt.q().Count(t.Context())
			if err != nil {
				t.Fatalf("Count: %v", err)
			}
			if (n > 0) != got {
				t.Errorf("Exists said %v but Count said %d", got, n)
			}
		})
	}
}

func TestOffset_integration(t *testing.T) {
	testdb.AdminDSN(t)
	d := db(t)

	page := func(limit, offset int) []string {
		users, err := d.Users.Query().
			OrderBy(gendemo.Users.ID.Asc()).
			Limit(limit).
			Offset(offset).
			All(t.Context())
		if err != nil {
			t.Fatalf("All: %v", err)
		}
		return emails(users)
	}

	if got := strings.Join(page(2, 0), ","); got != "alex@example.com,sam@example.com" {
		t.Errorf("the first page = %q", got)
	}
	if got := strings.Join(page(2, 2), ","); got != "robin@example.com" {
		t.Errorf("the second page = %q", got)
	}
	if got := page(2, 10); len(got) != 0 {
		t.Errorf("a page past the end = %q", got)
	}
}

func TestRows_integration(t *testing.T) {
	testdb.AdminDSN(t)
	d := db(t)

	t.Run("reads everything", func(t *testing.T) {
		var got []string
		for u, err := range d.Users.Query().OrderBy(gendemo.Users.ID.Asc()).Rows(t.Context()) {
			if err != nil {
				t.Fatalf("Rows: %v", err)
			}
			got = append(got, u.Email)
		}
		if want := strings.Join(emails(mustAll(t, d)), ","); strings.Join(got, ",") != want {
			t.Errorf("streamed %q, want %q", got, want)
		}
	})

	t.Run("stopping early releases the connection", func(t *testing.T) {
		// A single connection can carry one query at a time. If the first
		// stream were still open, the second would fail — so completing it is
		// the proof that breaking out closed the rows.
		var seen int
		for _, err := range d.Users.Query().OrderBy(gendemo.Users.ID.Asc()).Rows(t.Context()) {
			if err != nil {
				t.Fatalf("Rows: %v", err)
			}
			seen++
			break
		}
		if seen != 1 {
			t.Fatalf("read %d rows before breaking", seen)
		}
		if _, err := d.Users.Query().All(t.Context()); err != nil {
			t.Fatalf("the connection was not usable after an early break: %v", err)
		}
	})

	t.Run("a condition applies", func(t *testing.T) {
		var got []string
		for u, err := range d.Users.Query().
			Where(gendemo.Users.Active.Eq(true)).
			OrderBy(gendemo.Users.ID.Asc()).
			Rows(t.Context()) {
			if err != nil {
				t.Fatalf("Rows: %v", err)
			}
			got = append(got, u.Email)
		}
		if want := "alex@example.com,sam@example.com"; strings.Join(got, ",") != want {
			t.Errorf("streamed %q, want %q", got, want)
		}
	})

	t.Run("nothing to stream", func(t *testing.T) {
		for range d.Users.Query().Where(gendemo.Users.Email.Eq("nobody@example.com")).Rows(t.Context()) {
			t.Fatal("the iterator yielded a row for a query that matches none")
		}
	})
}

func mustAll(t *testing.T, d *gendemo.DB) []gendemo.User {
	t.Helper()
	users, err := d.Users.Query().OrderBy(gendemo.Users.ID.Asc()).All(t.Context())
	if err != nil {
		t.Fatalf("All: %v", err)
	}
	return users
}

func TestForUpdate_integration(t *testing.T) {
	testdb.AdminDSN(t)
	dsn := testdb.Create(t, schema(t)+seed)

	conn, err := pgx.Connect(t.Context(), dsn)
	if err != nil {
		t.Fatalf("connecting: %v", err)
	}
	defer func() { _ = conn.Close(context.WithoutCancel(t.Context())) }()

	tx, err := conn.Begin(t.Context())
	if err != nil {
		t.Fatalf("beginning a transaction: %v", err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(t.Context())) }()

	// FOR UPDATE outside a transaction releases the lock immediately, so the
	// clause is only meaningful in one.
	q := gendemo.New(tx).Users.Query().
		Where(gendemo.Users.Active.Eq(true)).
		OrderBy(gendemo.Users.ID.Asc()).
		Limit(2).
		Offset(0).
		ForUpdate()

	sql, _, err := q.SQL()
	if err != nil {
		t.Fatalf("SQL: %v", err)
	}
	if !strings.HasSuffix(sql, "LIMIT 2 OFFSET 0 FOR UPDATE") {
		t.Errorf("the clause order is wrong: %s", sql)
	}

	users, err := q.All(t.Context())
	if err != nil {
		t.Fatalf("All: %v", err)
	}
	if len(users) != 2 {
		t.Errorf("locked %d rows, want 2", len(users))
	}
}

func TestExpr_integration(t *testing.T) {
	testdb.AdminDSN(t)
	d := db(t)

	t.Run("mixed with typed predicates", func(t *testing.T) {
		users, err := d.Users.Query().
			Where(
				gendemo.Users.Active.Eq(true),
				orm.Expr[gendemo.User]("age > $1", 20),
			).
			OrderBy(gendemo.Users.ID.Asc()).
			All(t.Context())
		if err != nil {
			t.Fatalf("All: %v", err)
		}
		if got := strings.Join(emails(users), ","); got != "alex@example.com" {
			t.Errorf("matched %q", got)
		}
	})

	t.Run("one argument referred to twice", func(t *testing.T) {
		// alex is exactly 30, so this excludes exactly one row and proves the
		// single bound argument reached both sides of the OR.
		users, err := d.Users.Query().
			Where(orm.Expr[gendemo.User]("age < $1 OR age > $1", 30)).
			OrderBy(gendemo.Users.ID.Asc()).
			All(t.Context())
		if err != nil {
			t.Fatalf("All: %v", err)
		}
		if got := strings.Join(emails(users), ","); got != "sam@example.com,robin@example.com" {
			t.Errorf("matched %q", got)
		}

		sql, args, err := d.Users.Query().
			Where(orm.Expr[gendemo.User]("age < $1 OR age > $1", 30)).
			SQL()
		if err != nil {
			t.Fatalf("SQL: %v", err)
		}
		if !strings.Contains(sql, "age < $1 OR age > $1") {
			t.Errorf("the repeated reference was not preserved: %s", sql)
		}
		if len(args) != 1 {
			t.Errorf("a repeated reference bound %d arguments, want 1", len(args))
		}
	})

	t.Run("a fragment PostgreSQL understands and the typed API cannot say", func(t *testing.T) {
		// A jsonb path test is exactly what the escape hatch is for.
		users, err := d.Users.Query().
			Where(orm.Expr[gendemo.User](`settings ->> 'tier' = $1`, "gold")).
			All(t.Context())
		if err != nil {
			t.Fatalf("All: %v", err)
		}
		if got := strings.Join(emails(users), ","); got != "alex@example.com" {
			t.Errorf("matched %q", got)
		}
	})

	t.Run("values are still parameters", func(t *testing.T) {
		users, err := d.Users.Query().
			Where(orm.Expr[gendemo.User]("email = $1", `'; DROP TABLE users; --`)).
			All(t.Context())
		if err != nil {
			t.Fatalf("All: %v", err)
		}
		if len(users) != 0 {
			t.Errorf("matched %d rows", len(users))
		}
		if n, err := d.Users.Query().Count(t.Context()); err != nil || n != 3 {
			t.Errorf("the table holds %d rows (err %v), want 3", n, err)
		}
	})

	t.Run("a malformed fragment never reaches the server", func(t *testing.T) {
		_, err := d.Users.Query().
			Where(orm.Expr[gendemo.User]("age > $2", 20)).
			All(t.Context())
		if !errors.Is(err, orm.ErrRawPlaceholder) {
			t.Fatalf("error = %v, want ErrRawPlaceholder", err)
		}
	})
}

func TestAlias_integration(t *testing.T) {
	testdb.AdminDSN(t)
	d := db(t)

	manager := gendemo.Users.As("manager")

	t.Run("a query through the alias", func(t *testing.T) {
		sql, _, err := d.Users.QueryFrom(manager.Source()).
			Where(manager.Email.Eq("alex@example.com")).
			SQL()
		if err != nil {
			t.Fatalf("SQL: %v", err)
		}
		if !strings.Contains(sql, `FROM "public"."users" AS "manager"`) {
			t.Errorf("the alias did not reach the FROM clause: %s", sql)
		}
		if !strings.Contains(sql, `"manager"."email" = $1`) {
			t.Errorf("the condition did not qualify against the alias: %s", sql)
		}

		users, err := d.Users.QueryFrom(manager.Source()).
			Where(manager.Email.Eq("alex@example.com")).
			All(t.Context())
		if err != nil {
			t.Fatalf("All: %v", err)
		}
		if len(users) != 1 || users[0].ID != 1 {
			t.Errorf("the aliased query returned %+v", users)
		}
	})

	t.Run("As rebuilds every descriptor", func(t *testing.T) {
		// If As replaced only the table-level source and left the columns
		// pointing at the original, scope validation would be meaningless.
		if got := manager.ID.Source(); got != manager.Source() {
			t.Errorf("the aliased id belongs to %v, want the alias", got)
		}
		if got := manager.Email.Source(); got != manager.Source() {
			t.Errorf("the aliased email belongs to %v, want the alias", got)
		}
		if got := manager.CreatedAt.Source(); got != manager.Source() {
			t.Errorf("the aliased created_at belongs to %v, want the alias", got)
		}
		// And the original is untouched.
		if got := gendemo.Users.ID.Source(); got != gendemo.Users.Source() {
			t.Errorf("the unaliased id belongs to %v", got)
		}
		if manager.Source() == gendemo.Users.Source() {
			t.Error("the alias shares the original occurrence")
		}
	})

	t.Run("a column outside the scope is refused", func(t *testing.T) {
		_, err := d.Users.QueryFrom(manager.Source()).
			Where(gendemo.Users.Email.Eq("alex@example.com")).
			All(t.Context())
		if err == nil {
			t.Fatal("the query accepted a column from a table it does not select from")
		}
		var scopeErr *orm.ScopeError
		if !errors.As(err, &scopeErr) {
			t.Fatalf("error = %v (%T), want a *orm.ScopeError", err, err)
		}
	})

	t.Run("an invalid alias never reaches the server", func(t *testing.T) {
		for _, alias := range []string{"", "_reserved"} {
			bad := gendemo.Users.As(alias)
			if _, err := d.Users.QueryFrom(bad.Source()).All(t.Context()); err == nil {
				t.Errorf("the alias %q was accepted", alias)
			}
		}
	})
}

func TestM2FlagshipStillWorks(t *testing.T) {
	testdb.AdminDSN(t)
	d := db(t)

	// The query M2 was built around, unchanged.
	type filter struct {
		Email  string
		MinAge *int32
	}
	search := func(f filter) []string {
		predicates := []orm.Predicate[gendemo.User]{}
		if f.Email != "" {
			predicates = append(predicates, gendemo.Users.Email.ILike("%"+f.Email+"%"))
		}
		if f.MinAge != nil {
			predicates = append(predicates, gendemo.Users.Age.Gte(*f.MinAge))
		}
		users, err := d.Users.Query().
			Where(orm.And(predicates...)).
			OrderBy(gendemo.Users.CreatedAt.Desc()).
			Limit(50).
			All(t.Context())
		if err != nil {
			t.Fatalf("All: %v", err)
		}
		return emails(users)
	}

	if got := strings.Join(search(filter{}), ","); got != "robin@example.com,sam@example.com,alex@example.com" {
		t.Errorf("no filters matched %q", got)
	}
	if got := strings.Join(search(filter{Email: "ALEX"}), ","); got != "alex@example.com" {
		t.Errorf("an email filter matched %q", got)
	}
	if got := strings.Join(search(filter{MinAge: ptrTo(int32(30))}), ","); got != "robin@example.com,alex@example.com" {
		t.Errorf("an age filter matched %q", got)
	}
}

func ptrTo[T any](v T) *T { return &v }
