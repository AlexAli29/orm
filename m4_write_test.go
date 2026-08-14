package orm_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/AlexAli29/orm"
)

// writeMeta mirrors userMeta but carries the write-side flags: id is an
// identity column, slug is generated, created_at has a default, and nickname is
// nullable with none.
type W struct {
	ID        int64
	Email     string
	Age       int32
	Active    bool
	Nickname  *string
	Slug      string
	CreatedAt time.Time
}

var (
	wSrc = orm.NewSource("public", "w")

	Ws = struct {
		ID        orm.OrdCol[W, int64]
		Email     orm.TextCol[W]
		Age       orm.OrdCol[W, int32]
		Active    orm.Col[W, bool]
		Nickname  orm.NullTextCol[W]
		Slug      orm.TextCol[W]
		CreatedAt orm.OrdCol[W, time.Time]
	}{
		ID:        orm.NewOrdCol[W, int64](wSrc, "id"),
		Email:     orm.NewTextCol[W](wSrc, "email"),
		Age:       orm.NewOrdCol[W, int32](wSrc, "age"),
		Active:    orm.NewCol[W, bool](wSrc, "active"),
		Nickname:  orm.NewNullTextCol[W](wSrc, "nickname"),
		Slug:      orm.NewTextCol[W](wSrc, "slug"),
		CreatedAt: orm.NewOrdCol[W, time.Time](wSrc, "created_at"),
	}

	wMeta = orm.EntityMeta[W]{
		Table:  orm.TableID{Schema: "public", Name: "w"},
		Source: wSrc,
		Columns: []orm.ColumnMeta{
			{Name: "id", Field: "ID", NotNull: true, Identity: true},
			{Name: "email", Field: "Email", NotNull: true},
			{Name: "age", Field: "Age", NotNull: true},
			{Name: "active", Field: "Active", NotNull: true, HasDefault: true},
			{Name: "nickname", Field: "Nickname"},
			{Name: "slug", Field: "Slug", NotNull: true, Generated: true},
			{Name: "created_at", Field: "CreatedAt", NotNull: true, HasDefault: true},
		},
		Dest: func(w *W, idx int) any {
			switch idx {
			case 0:
				return &w.ID
			case 1:
				return &w.Email
			case 2:
				return &w.Age
			case 3:
				return &w.Active
			case 4:
				return &w.Nickname
			case 5:
				return &w.Slug
			case 6:
				return &w.CreatedAt
			}
			return nil
		},
		Value: func(w *W, idx int) any {
			switch idx {
			case 0:
				return w.ID
			case 1:
				return w.Email
			case 2:
				return w.Age
			case 3:
				return w.Active
			case 4:
				return w.Nickname
			case 5:
				return w.Slug
			case 6:
				return w.CreatedAt
			}
			return nil
		},
	}
)

// returningAll is the RETURNING list every insert carries: every mapped column,
// in the order the scanner indexes into.
const returningAll = ` RETURNING "id", "email", "age", "active", "nickname", "slug", "created_at"`

func wRepo(ex orm.Executor) *orm.Repo[W] { return orm.NewRepo(ex, &wMeta) }

func sample() W {
	return W{Email: "a@example.com", Age: 30, Active: false, CreatedAt: epoch}
}

func TestInsertSQL(t *testing.T) {
	tests := []struct {
		name string
		opts []orm.InsertOpt[W]
		sql  string
		args []any
	}{
		{
			// The identity column and the generated column are absent from the
			// write list and present in RETURNING. Active is false and is
			// written as false.
			name: "every writable column, zero values included",
			sql: `INSERT INTO "public"."w" ("email", "age", "active", "nickname", "created_at")` +
				` VALUES ($1, $2, $3, $4, $5)` + returningAll,
			args: []any{"a@example.com", int32(30), false, (*string)(nil), epoch},
		},
		{
			name: "a column left to the database",
			opts: []orm.InsertOpt[W]{orm.Default(Ws.Active)},
			sql: `INSERT INTO "public"."w" ("email", "age", "nickname", "created_at")` +
				` VALUES ($1, $2, $3, $4)` + returningAll,
			args: []any{"a@example.com", int32(30), (*string)(nil), epoch},
		},
		{
			name: "several columns left to the database",
			opts: []orm.InsertOpt[W]{orm.Default(Ws.Active, Ws.CreatedAt)},
			sql: `INSERT INTO "public"."w" ("email", "age", "nickname")` +
				` VALUES ($1, $2, $3)` + returningAll,
			args: []any{"a@example.com", int32(30), (*string)(nil)},
		},
		{
			// A nullable column with no default can be left out too: its
			// default is NULL.
			name: "a nullable column left to the database",
			opts: []orm.InsertOpt[W]{orm.Default(Ws.Nickname)},
			sql: `INSERT INTO "public"."w" ("email", "age", "active", "created_at")` +
				` VALUES ($1, $2, $3, $4)` + returningAll,
			args: []any{"a@example.com", int32(30), false, epoch},
		},
		{
			name: "do nothing on conflict",
			opts: []orm.InsertOpt[W]{orm.OnConflict(Ws.Email).DoNothing()},
			sql: `INSERT INTO "public"."w" ("email", "age", "active", "nickname", "created_at")` +
				` VALUES ($1, $2, $3, $4, $5) ON CONFLICT ("email") DO NOTHING` + returningAll,
			args: []any{"a@example.com", int32(30), false, (*string)(nil), epoch},
		},
		{
			name: "update the named columns on conflict",
			opts: []orm.InsertOpt[W]{orm.OnConflict(Ws.Email).DoUpdate(Ws.Age, Ws.Active)},
			sql: `INSERT INTO "public"."w" ("email", "age", "active", "nickname", "created_at")` +
				` VALUES ($1, $2, $3, $4, $5) ON CONFLICT ("email")` +
				` DO UPDATE SET "age" = EXCLUDED."age", "active" = EXCLUDED."active"` + returningAll,
			args: []any{"a@example.com", int32(30), false, (*string)(nil), epoch},
		},
		{
			name: "a composite conflict target",
			opts: []orm.InsertOpt[W]{orm.OnConflict(Ws.Email, Ws.Age).DoNothing()},
			sql: `INSERT INTO "public"."w" ("email", "age", "active", "nickname", "created_at")` +
				` VALUES ($1, $2, $3, $4, $5) ON CONFLICT ("email", "age") DO NOTHING` + returningAll,
			args: []any{"a@example.com", int32(30), false, (*string)(nil), epoch},
		},
		{
			name: "a default and a conflict clause together",
			opts: []orm.InsertOpt[W]{
				orm.Default(Ws.CreatedAt),
				orm.OnConflict(Ws.Email).DoUpdate(Ws.Age),
			},
			sql: `INSERT INTO "public"."w" ("email", "age", "active", "nickname")` +
				` VALUES ($1, $2, $3, $4) ON CONFLICT ("email")` +
				` DO UPDATE SET "age" = EXCLUDED."age"` + returningAll,
			args: []any{"a@example.com", int32(30), false, (*string)(nil)},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sql, args, err := wRepo(stubExecutor{}).InsertSQL([]W{sample()}, tt.opts...)
			if err != nil {
				t.Fatalf("InsertSQL: %v", err)
			}
			if sql != tt.sql {
				t.Errorf("sql = %s\nwant %s", sql, tt.sql)
			}
			assertArgs(t, args, tt.args)
		})
	}
}

func TestInsertSQL_multipleRows(t *testing.T) {
	rows := []W{
		{Email: "a@example.com", Age: 1, CreatedAt: epoch},
		{Email: "b@example.com", Age: 2, Active: true, CreatedAt: epoch},
	}
	sql, args, err := wRepo(stubExecutor{}).InsertSQL(rows, orm.Default(Ws.CreatedAt))
	if err != nil {
		t.Fatalf("InsertSQL: %v", err)
	}
	want := `INSERT INTO "public"."w" ("email", "age", "active", "nickname")` +
		` VALUES ($1, $2, $3, $4), ($5, $6, $7, $8)` + returningAll
	if sql != want {
		t.Errorf("sql = %s\nwant %s", sql, want)
	}
	assertArgs(t, args, []any{
		"a@example.com", int32(1), false, (*string)(nil),
		"b@example.com", int32(2), true, (*string)(nil),
	})
}

func TestInsert_nullIsNotDefault(t *testing.T) {
	// A nil nullable field writes NULL. Only Default omits the column.
	sql, args, err := wRepo(stubExecutor{}).InsertSQL([]W{sample()})
	if err != nil {
		t.Fatalf("InsertSQL: %v", err)
	}
	if !strings.Contains(sql, `"nickname"`) {
		t.Errorf("a nil nullable field dropped its column: %s", sql)
	}
	if args[3] != (*string)(nil) {
		t.Errorf("args[3] = %#v, want a nil *string", args[3])
	}

	sql, _, err = wRepo(stubExecutor{}).InsertSQL([]W{sample()}, orm.Default(Ws.Nickname))
	if err != nil {
		t.Fatalf("InsertSQL: %v", err)
	}
	if strings.Contains(sql, `"nickname"`) && !strings.Contains(sql, `RETURNING "id", "email", "age", "active", "nickname"`) {
		t.Errorf("Default did not omit the column: %s", sql)
	}
}

func TestInsert_invalidConfiguration(t *testing.T) {
	tests := []struct {
		name string
		opts []orm.InsertOpt[W]
		is   error
		want string
	}{
		{
			// Omitting it would make PostgreSQL reject the row, and its
			// message would name a constraint rather than this call.
			name: "a default for a column that has none",
			opts: []orm.InsertOpt[W]{orm.Default(Ws.Email)},
			is:   orm.ErrInvalidDefault,
			want: "NOT NULL with no default",
		},
		{
			name: "the same column defaulted twice",
			opts: []orm.InsertOpt[W]{orm.Default(Ws.Active, Ws.Active)},
			is:   orm.ErrInvalidDefault,
			want: "listed twice",
		},
		{
			name: "two conflict clauses",
			opts: []orm.InsertOpt[W]{
				orm.OnConflict(Ws.Email).DoNothing(),
				orm.OnConflict(Ws.Age).DoNothing(),
			},
			want: "more than one OnConflict",
		},
		{
			name: "updating a generated column on conflict",
			opts: []orm.InsertOpt[W]{orm.OnConflict(Ws.Email).DoUpdate(Ws.Slug)},
			want: "computes and will not accept a value",
		},
		{
			name: "updating an identity column on conflict",
			opts: []orm.InsertOpt[W]{orm.OnConflict(Ws.Email).DoUpdate(Ws.ID)},
			want: "identity column",
		},
		{
			name: "the same column updated twice on conflict",
			opts: []orm.InsertOpt[W]{orm.OnConflict(Ws.Email).DoUpdate(Ws.Age, Ws.Age)},
			is:   orm.ErrDuplicateAssignment,
			want: "twice",
		},
		{
			name: "do update naming nothing",
			opts: []orm.InsertOpt[W]{orm.OnConflict(Ws.Email).DoUpdate()},
			want: "use DoNothing",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ex := &counting{}
			_, err := wRepo(ex).Insert(context.Background(), sample(), tt.opts...)
			if err == nil {
				t.Fatal("Insert accepted the configuration")
			}
			if tt.is != nil && !errors.Is(err, tt.is) {
				t.Errorf("error = %v, want it to wrap %v", err, tt.is)
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("error = %v, want it to contain %q", err, tt.want)
			}
			if ex.calls != 0 {
				t.Errorf("the executor was called %d times", ex.calls)
			}
		})
	}
}

func TestUpdateSQL(t *testing.T) {
	tests := []struct {
		name string
		u    func() *orm.Update[W]
		sql  string
		args []any
	}{
		{
			name: "one assignment",
			u: func() *orm.Update[W] {
				return wRepo(stubExecutor{}).Update().Set(Ws.Email.Set("b@example.com")).Where(Ws.ID.Eq(1))
			},
			sql:  `UPDATE "public"."w" SET "email" = $1 WHERE "w"."id" = $2`,
			args: []any{"b@example.com", int64(1)},
		},
		{
			// The SET list comes first, so its parameters are numbered before
			// the ones in WHERE.
			name: "several assignments",
			u: func() *orm.Update[W] {
				return wRepo(stubExecutor{}).Update().
					Set(Ws.Email.Set("b@example.com"), Ws.Active.Set(true)).
					Where(Ws.Age.Gte(int32(18)))
			},
			sql:  `UPDATE "public"."w" SET "email" = $1, "active" = $2 WHERE "w"."age" >= $3`,
			args: []any{"b@example.com", true, int32(18)},
		},
		{
			name: "assigning NULL binds nothing",
			u: func() *orm.Update[W] {
				return wRepo(stubExecutor{}).Update().Set(Ws.Nickname.SetNull()).Where(Ws.ID.Eq(1))
			},
			sql:  `UPDATE "public"."w" SET "nickname" = NULL WHERE "w"."id" = $1`,
			args: []any{int64(1)},
		},
		{
			name: "assigning a value to a nullable column binds it",
			u: func() *orm.Update[W] {
				return wRepo(stubExecutor{}).Update().Set(Ws.Nickname.Set("alex")).Where(Ws.ID.Eq(1))
			},
			sql:  `UPDATE "public"."w" SET "nickname" = $1 WHERE "w"."id" = $2`,
			args: []any{"alex", int64(1)},
		},
		{
			name: "several predicates combine with AND",
			u: func() *orm.Update[W] {
				return wRepo(stubExecutor{}).Update().
					Set(Ws.Active.Set(false)).
					Where(Ws.Age.Lt(int32(18)), Ws.Active.Eq(true))
			},
			sql:  `UPDATE "public"."w" SET "active" = $1 WHERE "w"."age" < $2 AND "w"."active" = $3`,
			args: []any{false, int32(18), true},
		},
		{
			name: "every row, deliberately",
			u: func() *orm.Update[W] {
				return wRepo(stubExecutor{}).Update().Set(Ws.Active.Set(false)).All()
			},
			sql:  `UPDATE "public"."w" SET "active" = $1`,
			args: []any{false},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sql, args, err := tt.u().SQL()
			if err != nil {
				t.Fatalf("SQL: %v", err)
			}
			if sql != tt.sql {
				t.Errorf("sql = %s\nwant %s", sql, tt.sql)
			}
			assertArgs(t, args, tt.args)
		})
	}
}

func TestDeleteSQL(t *testing.T) {
	tests := []struct {
		name string
		d    func() *orm.Delete[W]
		sql  string
		args []any
	}{
		{
			name: "by identity",
			d:    func() *orm.Delete[W] { return wRepo(stubExecutor{}).Delete().Where(Ws.ID.Eq(1)) },
			sql:  `DELETE FROM "public"."w" WHERE "w"."id" = $1`,
			args: []any{int64(1)},
		},
		{
			name: "several predicates",
			d: func() *orm.Delete[W] {
				return wRepo(stubExecutor{}).Delete().Where(Ws.Active.Eq(false), Ws.Age.Lt(int32(18)))
			},
			sql:  `DELETE FROM "public"."w" WHERE "w"."active" = $1 AND "w"."age" < $2`,
			args: []any{false, int32(18)},
		},
		{
			name: "every row, deliberately",
			d:    func() *orm.Delete[W] { return wRepo(stubExecutor{}).Delete().All() },
			sql:  `DELETE FROM "public"."w"`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sql, args, err := tt.d().SQL()
			if err != nil {
				t.Fatalf("SQL: %v", err)
			}
			if sql != tt.sql {
				t.Errorf("sql = %s\nwant %s", sql, tt.sql)
			}
			assertArgs(t, args, tt.args)
		})
	}
}

func TestWriteGuards(t *testing.T) {
	// Every one of these has to be caught before a statement is sent. A
	// full-table write is among the few mistakes a person cannot undo.
	tests := []struct {
		name string
		run  func(*orm.Repo[W]) error
		is   error
		want string
	}{
		{
			name: "an update with no WHERE",
			run: func(r *orm.Repo[W]) error {
				_, err := r.Update().Set(Ws.Active.Set(false)).Exec(context.Background())
				return err
			},
			is:   orm.ErrMissingWhere,
			want: "call All()",
		},
		{
			name: "an update that assigns nothing",
			run: func(r *orm.Repo[W]) error {
				_, err := r.Update().Where(Ws.ID.Eq(1)).Exec(context.Background())
				return err
			},
			is: orm.ErrMissingSet,
		},
		{
			name: "an update with neither",
			run: func(r *orm.Repo[W]) error {
				_, err := r.Update().Exec(context.Background())
				return err
			},
			is: orm.ErrMissingSet,
		},
		{
			name: "a delete with no WHERE",
			run: func(r *orm.Repo[W]) error {
				_, err := r.Delete().Exec(context.Background())
				return err
			},
			is:   orm.ErrMissingWhere,
			want: "call All()",
		},
		{
			name: "the same column assigned twice",
			run: func(r *orm.Repo[W]) error {
				_, err := r.Update().
					Set(Ws.Email.Set("a"), Ws.Email.Set("b")).
					Where(Ws.ID.Eq(1)).
					Exec(context.Background())
				return err
			},
			is:   orm.ErrDuplicateAssignment,
			want: "email",
		},
		{
			name: "assigning a generated column",
			run: func(r *orm.Repo[W]) error {
				_, err := r.Update().Set(Ws.Slug.Set("x")).Where(Ws.ID.Eq(1)).Exec(context.Background())
				return err
			},
			want: "computes and will not accept a value",
		},
		{
			name: "assigning an identity column",
			run: func(r *orm.Repo[W]) error {
				_, err := r.Update().Set(Ws.ID.Set(1)).Where(Ws.Email.Eq("a")).Exec(context.Background())
				return err
			},
			want: "identity column",
		},
		{
			name: "All after Where on an update",
			run: func(r *orm.Repo[W]) error {
				_, err := r.Update().Set(Ws.Active.Set(false)).Where(Ws.ID.Eq(1)).All().Exec(context.Background())
				return err
			},
			want: "All was called after Where",
		},
		{
			name: "Where after All on an update",
			run: func(r *orm.Repo[W]) error {
				_, err := r.Update().Set(Ws.Active.Set(false)).All().Where(Ws.ID.Eq(1)).Exec(context.Background())
				return err
			},
			want: "Where was called after All",
		},
		{
			name: "All after Where on a delete",
			run: func(r *orm.Repo[W]) error {
				_, err := r.Delete().Where(Ws.ID.Eq(1)).All().Exec(context.Background())
				return err
			},
			want: "All was called after Where",
		},
		{
			name: "Where after All on a delete",
			run: func(r *orm.Repo[W]) error {
				_, err := r.Delete().All().Where(Ws.ID.Eq(1)).Exec(context.Background())
				return err
			},
			want: "Where was called after All",
		},
		{
			name: "a malformed raw predicate on a delete",
			run: func(r *orm.Repo[W]) error {
				_, err := r.Delete().Where(orm.Expr[W]("x > $2", 1)).Exec(context.Background())
				return err
			},
			is: orm.ErrRawPlaceholder,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ex := &counting{}
			err := tt.run(wRepo(ex))
			if err == nil {
				t.Fatal("the write was accepted")
			}
			if tt.is != nil && !errors.Is(err, tt.is) {
				t.Errorf("error = %v, want it to wrap %v", err, tt.is)
			}
			if tt.want != "" && !strings.Contains(err.Error(), tt.want) {
				t.Errorf("error = %v, want it to contain %q", err, tt.want)
			}
			if ex.calls != 0 {
				t.Errorf("the executor was called %d times for a write that never compiled", ex.calls)
			}
		})
	}
}

func TestWriteGuards_alsoStopSQL(t *testing.T) {
	// SQL is the same build path, so it refuses the same writes.
	if _, _, err := wRepo(stubExecutor{}).Update().Set(Ws.Active.Set(false)).SQL(); !errors.Is(err, orm.ErrMissingWhere) {
		t.Errorf("Update.SQL error = %v, want ErrMissingWhere", err)
	}
	if _, _, err := wRepo(stubExecutor{}).Delete().SQL(); !errors.Is(err, orm.ErrMissingWhere) {
		t.Errorf("Delete.SQL error = %v, want ErrMissingWhere", err)
	}
	if _, _, err := wRepo(stubExecutor{}).Update().Where(Ws.ID.Eq(1)).SQL(); !errors.Is(err, orm.ErrMissingSet) {
		t.Errorf("Update.SQL error = %v, want ErrMissingSet", err)
	}
}

func TestInsertMany_empty(t *testing.T) {
	for _, in := range [][]W{nil, {}} {
		ex := &counting{}
		out, err := wRepo(ex).InsertMany(context.Background(), in)
		if err != nil {
			t.Fatalf("InsertMany: %v", err)
		}
		if len(out) != 0 {
			t.Errorf("returned %d rows", len(out))
		}
		if ex.calls != 0 {
			t.Errorf("the executor was called %d times for an empty insert", ex.calls)
		}
	}
}

func TestIdentifierQuotingOnWrites(t *testing.T) {
	src := orm.NewSource("we ird", `ta"ble`)
	meta := orm.EntityMeta[W]{
		Table:   orm.TableID{Schema: "we ird", Name: `ta"ble`},
		Source:  src,
		Columns: []orm.ColumnMeta{{Name: `co"l`, Field: "Email", NotNull: true}},
		Dest:    func(w *W, idx int) any { return &w.Email },
		Value:   func(w *W, idx int) any { return w.Email },
	}
	r := orm.NewRepo(stubExecutor{}, &meta)
	col := orm.NewTextCol[W](src, `co"l`)

	sql, _, err := r.InsertSQL([]W{{Email: "x"}})
	if err != nil {
		t.Fatalf("InsertSQL: %v", err)
	}
	const wantInsert = `INSERT INTO "we ird"."ta""ble" ("co""l") VALUES ($1) RETURNING "co""l"`
	if sql != wantInsert {
		t.Errorf("insert = %s\nwant %s", sql, wantInsert)
	}

	sql, _, err = r.Update().Set(col.Set("y")).Where(col.Eq("x")).SQL()
	if err != nil {
		t.Fatalf("SQL: %v", err)
	}
	const wantUpdate = `UPDATE "we ird"."ta""ble" SET "co""l" = $1 WHERE "ta""ble"."co""l" = $2`
	if sql != wantUpdate {
		t.Errorf("update = %s\nwant %s", sql, wantUpdate)
	}
}
