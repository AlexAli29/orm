package gendemo_test

import (
	"errors"
	"testing"

	"github.com/AlexAli29/orm"
	"github.com/AlexAli29/orm/internal/gendemo"
	"github.com/AlexAli29/orm/internal/testdb"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// M12.3: JSONB.
//
// The claim under test is that PostgreSQL's three kinds of nothing stay apart —
// a SQL NULL column, a JSON null value, and a key that is not there — and that
// every operator's result is the type this package says it is.

// jsonRows seeds the five documents the NULL matrix is written against.
func jsonRows(t *testing.T, conn *pgx.Conn) {
	t.Helper()
	m11exec(t, conn, `DELETE FROM posts; DELETE FROM users`)
	m11exec(t, conn, `INSERT INTO users (id, email, age, state, tags, settings, metadata, created_at) VALUES
	    (1, 'sqlnull@example.com',  1, 'active', '{}', '{}', NULL,          now()),
	    (2, 'empty@example.com',    2, 'active', '{}', '{}', '{}',          now()),
	    (3, 'jsonnull@example.com', 3, 'active', '{}', '{}', '{"x": null}', now()),
	    (4, 'zero@example.com',     4, 'active', '{}', '{}', '{"x": 0}',    now()),
	    (5, 'blank@example.com',    5, 'active', '{}', '{}', '{"x": ""}',   now())`)
}

// Release-critical: SQL NULL, JSON null and a missing key are three different
// answers, and each operator gives PostgreSQL's.
func TestJSONB_nullMatrix(t *testing.T) {
	testdb.AdminDSN(t)
	db, conn := m12env(t)
	jsonRows(t, conn)

	// metadata is the nullable document; row 1 has a SQL NULL for it.
	settings := gendemo.Users.Metadata

	t.Run("text extraction", func(t *testing.T) {
		// ->> is NULL for a SQL NULL document, NULL for a missing key, NULL for
		// a JSON null, "0" for the number and "" for the empty string. The last
		// two are what a Go zero would have erased.
		type row struct {
			ID int64
			X  *string
		}
		shape := orm.Project2(orm.Of(gendemo.Users.ID), orm.JSONText(settings, "x"),
			func(id int64, x *string) row { return row{ID: id, X: x} })
		got, err := orm.Compose(db.Executor(), shape).
			From(gendemo.Users.Source()).
			OrderBy(orm.Of(gendemo.Users.ID).Asc()).
			All(t.Context())
		if err != nil {
			t.Fatalf("All: %v", err)
		}
		want := []*string{nil, nil, nil, strptr("0"), strptr("")}
		if len(got) != len(want) {
			t.Fatalf("got %d rows", len(got))
		}
		for i, w := range want {
			switch {
			case (got[i].X == nil) != (w == nil):
				t.Errorf("row %d: ->> read %v, want %v", i+1, got[i].X, w)
			case w != nil && *got[i].X != *w:
				t.Errorf("row %d: ->> read %q, want %q", i+1, *got[i].X, *w)
			}
		}
		// The empty string and the number are present values, not absences.
		if got[4].X == nil || *got[4].X != "" {
			t.Error("an empty JSON string became a SQL NULL")
		}
	})

	t.Run("key existence tells JSON null from missing", func(t *testing.T) {
		// ? is TRUE for {"x": null}: the key is there, whatever its value.
		got := jsonIDs(t, db, orm.JSONHasKey(settings, "x"))
		assertIDs(t, conn, `SELECT id FROM users WHERE metadata ? 'x' ORDER BY id`, got)
		if len(got) != 3 {
			t.Errorf("? matched %v; the three documents with the key are 3, 4 and 5", got)
		}
	})

	t.Run("jsonb_typeof names the JSON null", func(t *testing.T) {
		type row struct {
			ID   int64
			Kind *string
		}
		shape := orm.Project2(orm.Of(gendemo.Users.ID),
			orm.JSONTypeOf(orm.JSONGet(settings, "x")),
			func(id int64, k *string) row { return row{ID: id, Kind: k} })
		got, err := orm.Compose(db.Executor(), shape).
			From(gendemo.Users.Source()).
			OrderBy(orm.Of(gendemo.Users.ID).Asc()).
			All(t.Context())
		if err != nil {
			t.Fatalf("All: %v", err)
		}
		// A missing key is SQL NULL; a JSON null reports "null".
		if got[1].Kind != nil {
			t.Errorf("a missing key reported type %q", *got[1].Kind)
		}
		if got[2].Kind == nil || *got[2].Kind != "null" {
			t.Errorf("a JSON null reported type %v, want \"null\"", got[2].Kind)
		}
		if got[3].Kind == nil || *got[3].Kind != "number" {
			t.Errorf("a JSON number reported type %v", got[3].Kind)
		}
	})

	t.Run("containment", func(t *testing.T) {
		got := jsonIDs(t, db, orm.JSONContains(settings, orm.Val(map[string]any{"x": 0})))
		assertIDs(t, conn, `SELECT id FROM users WHERE metadata @> '{"x":0}'::jsonb ORDER BY id`, got)

		got = jsonIDs(t, db, orm.JSONContainedBy(settings, orm.Val(map[string]any{"x": 0, "y": 1})))
		assertIDs(t, conn, `SELECT id FROM users WHERE metadata <@ '{"x":0,"y":1}'::jsonb ORDER BY id`, got)
	})

	t.Run("any and all keys", func(t *testing.T) {
		got := jsonIDs(t, db, orm.JSONHasAnyKeys(settings, "x", "y"))
		assertIDs(t, conn, `SELECT id FROM users WHERE metadata ?| ARRAY['x','y'] ORDER BY id`, got)

		got = jsonIDs(t, db, orm.JSONHasAllKeys(settings, "x"))
		assertIDs(t, conn, `SELECT id FROM users WHERE metadata ?& ARRAY['x'] ORDER BY id`, got)
	})
}

// Path extraction, with keys that would break a statement assembled as text.
func TestJSONB_paths(t *testing.T) {
	testdb.AdminDSN(t)
	db, conn := m12env(t)
	m11exec(t, conn, `DELETE FROM posts; DELETE FROM users`)
	m11exec(t, conn, `INSERT INTO users (id, email, age, state, tags, settings, created_at) VALUES
	    (1, 'p@example.com', 1, 'active', '{}',
	     '{"profile": {"age": 21, "a\"b": "quoted", "ünïcode": "yes", "": "empty key"}}', now())`)

	settings := gendemo.Users.Settings
	one := func(t *testing.T, v orm.Selectable[orm.Composed, *string]) *string {
		t.Helper()
		got, err := orm.Compose(db.Executor(), orm.Project1(v, func(s *string) *string { return s })).
			From(gendemo.Users.Source()).One(t.Context())
		if err != nil {
			t.Fatalf("One: %v", err)
		}
		return got
	}

	if got := one(t, orm.JSONPathText(settings, "profile", "age")); got == nil || *got != "21" {
		t.Errorf("#>> read %v", got)
	}
	// A key with a quote, a Unicode key and an empty key all travel as values.
	if got := one(t, orm.JSONPathText(settings, "profile", `a"b`)); got == nil || *got != "quoted" {
		t.Errorf("a key containing a quote read %v", got)
	}
	if got := one(t, orm.JSONPathText(settings, "profile", "ünïcode")); got == nil || *got != "yes" {
		t.Errorf("a Unicode key read %v", got)
	}
	if got := one(t, orm.JSONPathText(settings, "profile", "")); got == nil || *got != "empty key" {
		t.Errorf("an empty key read %v", got)
	}
	// A path that is not there is SQL NULL.
	if got := one(t, orm.JSONPathText(settings, "profile", "absent")); got != nil {
		t.Errorf("a missing path read %q", *got)
	}

	// #> keeps the value as jsonb.
	sub, err := orm.Compose(db.Executor(), orm.Project1(
		orm.JSONTypeOf(orm.JSONPathGet(settings, "profile")),
		func(s *string) *string { return s })).
		From(gendemo.Users.Source()).One(t.Context())
	if err != nil {
		t.Fatalf("One: %v", err)
	}
	if sub == nil || *sub != "object" {
		t.Errorf("#> of an object reported type %v", sub)
	}
}

// Scenario H: extraction, a cast and a comparison, composed from M11's CAST.
func TestJSONB_extractionWithCast(t *testing.T) {
	testdb.AdminDSN(t)
	db, conn := m12env(t)
	m11exec(t, conn, `DELETE FROM posts; DELETE FROM users`)
	m11exec(t, conn, `INSERT INTO users (id, email, age, state, tags, settings, created_at) VALUES
	    (1, 'adult@example.com', 1, 'active', '{}', '{"profile": {"age": 30}}', now()),
	    (2, 'minor@example.com', 2, 'active', '{}', '{"profile": {"age": 12}}', now()),
	    (3, 'none@example.com',  3, 'active', '{}', '{}',                       now())`)

	age := orm.CastNull(orm.JSONPathText(gendemo.Users.Settings, "profile", "age"), orm.Integer)
	got := jsonIDs(t, db, age.Gte(int32ptr(18)))
	assertIDs(t, conn, `
		SELECT id FROM users WHERE (settings #>> '{profile,age}')::int >= 18 ORDER BY id`, got)
	if len(got) != 1 || got[0] != 1 {
		t.Errorf("the cast comparison matched %v", got)
	}
}

// Scenario I: one statement that modifies a document and returns the result.
func TestJSONB_setAndInsertThroughAnUpdate(t *testing.T) {
	testdb.AdminDSN(t)
	db, conn := m12env(t)
	m11exec(t, conn, `DELETE FROM posts; DELETE FROM users`)
	m11exec(t, conn, `INSERT INTO users (id, email, age, state, tags, settings, created_at) VALUES
	    (1, 'u@example.com', 1, 'active', '{}', '{"profile": {"age": 30}}', now())`)

	// jsonb_set with a document value: the new value is itself jsonb.
	set := orm.JSONSet(gendemo.Users.Settings, []string{"profile", "verified"},
		map[string]any{"ok": true}, true)
	shape := orm.Project2(
		gendemo.Users.ID.As("id"),
		orm.Nullable(orm.RawValue[gendemo.User, string](`(settings #>> '{profile,verified,ok}')`)).As("v"),
		func(id int64, v *string) string {
			if v == nil {
				return "<nil>"
			}
			return *v
		},
	)
	got, err := orm.UpdateReturning(
		db.Users.Update().
			Set(gendemo.Users.Settings.SetExpr(set)).
			Where(gendemo.Users.ID.Eq(int64(1))),
		shape).All(t.Context())
	if err != nil {
		t.Fatalf("UpdateReturning: %v", err)
	}
	if len(got) != 1 || got[0] != "true" {
		t.Errorf("the returned document reported %v", got)
	}

	// jsonb_insert adds a path that is not there.
	ins := orm.JSONInsert(gendemo.Users.Settings, []string{"profile", "nickname"},
		map[string]any{"n": "ada"}, false)
	if _, err := db.Users.Update().
		Set(gendemo.Users.Settings.SetExpr(ins)).
		Where(gendemo.Users.ID.Eq(int64(1))).
		Exec(t.Context()); err != nil {
		t.Fatalf("jsonb_insert: %v", err)
	}
	var nick *string
	if err := conn.QueryRow(t.Context(),
		`SELECT settings #>> '{profile,nickname,n}' FROM users WHERE id = 1`).Scan(&nick); err != nil {
		t.Fatalf("reading back: %v", err)
	}
	if nick == nil || *nick != "ada" {
		t.Errorf("jsonb_insert did not add the path: %v", nick)
	}
	// And inserting where a key already exists is PostgreSQL's error, not a
	// quiet no-op.
	clash := orm.JSONInsert(gendemo.Users.Settings, []string{"profile", "age"},
		map[string]any{"nope": true}, false)
	_, err = db.Users.Update().
		Set(gendemo.Users.Settings.SetExpr(clash)).
		Where(gendemo.Users.ID.Eq(int64(1))).
		Exec(t.Context())
	if err == nil {
		t.Fatal("jsonb_insert replaced an existing key")
	}
	var pg *pgconn.PgError
	if !errors.As(err, &pg) {
		t.Errorf("error = %v, want a *pgconn.PgError", err)
	}
}

// JSONPath predicates, bound as values rather than spliced as syntax.
func TestJSONB_jsonPath(t *testing.T) {
	testdb.AdminDSN(t)
	db, conn := m12env(t)
	m11exec(t, conn, `DELETE FROM posts; DELETE FROM users`)
	m11exec(t, conn, `INSERT INTO users (id, email, age, state, tags, settings, created_at) VALUES
	    (1, 'a@example.com', 1, 'active', '{}', '{"n": 5}',  now()),
	    (2, 'b@example.com', 2, 'active', '{}', '{"n": 50}', now())`)

	got := jsonIDs(t, db, orm.JSONMatches(gendemo.Users.Settings, `$.n > 10`))
	assertIDs(t, conn, `SELECT id FROM users WHERE settings @@ '$.n > 10'::jsonpath ORDER BY id`, got)

	got = jsonIDs(t, db, orm.JSONPathExists(gendemo.Users.Settings, `$.n ? (@ > 10)`))
	assertIDs(t, conn, `SELECT id FROM users WHERE settings @? '$.n ? (@ > 10)'::jsonpath ORDER BY id`, got)
}

// Every jsonb result type this package claims, against the server.
func TestPGTypeOf_M12JSONB(t *testing.T) {
	testdb.AdminDSN(t)
	_, conn := m12env(t)

	for _, tt := range []struct{ what, sql, want string }{
		{"->", `SELECT pg_typeof(settings -> 'x') FROM users LIMIT 1`, "jsonb"},
		{"->>", `SELECT pg_typeof(settings ->> 'x') FROM users LIMIT 1`, "text"},
		{"#>", `SELECT pg_typeof(settings #> ARRAY['a','b']) FROM users LIMIT 1`, "jsonb"},
		{"#>>", `SELECT pg_typeof(settings #>> ARRAY['a','b']) FROM users LIMIT 1`, "text"},
		{"@>", `SELECT pg_typeof(settings @> '{}'::jsonb) FROM users LIMIT 1`, "boolean"},
		{"<@", `SELECT pg_typeof(settings <@ '{}'::jsonb) FROM users LIMIT 1`, "boolean"},
		{"?|", `SELECT pg_typeof(settings ?| ARRAY['x']) FROM users LIMIT 1`, "boolean"},
		{"?&", `SELECT pg_typeof(settings ?& ARRAY['x']) FROM users LIMIT 1`, "boolean"},
		{"jsonb_set", `SELECT pg_typeof(jsonb_set(settings, ARRAY['x'], '1'::jsonb, true)) FROM users LIMIT 1`, "jsonb"},
		{"jsonb_insert", `SELECT pg_typeof(jsonb_insert(settings, ARRAY['x'], '1'::jsonb, false)) FROM users LIMIT 1`, "jsonb"},
		{"jsonb_strip_nulls", `SELECT pg_typeof(jsonb_strip_nulls(settings)) FROM users LIMIT 1`, "jsonb"},
		{"jsonb_typeof", `SELECT pg_typeof(jsonb_typeof(settings)) FROM users LIMIT 1`, "text"},
		{"jsonb_array_length", `SELECT pg_typeof(jsonb_array_length('[1,2]'::jsonb))`, "integer"},
		{"@@", `SELECT pg_typeof(settings @@ '$.x == 1'::jsonpath) FROM users LIMIT 1`, "boolean"},
		{"@?", `SELECT pg_typeof(settings @? '$.x'::jsonpath) FROM users LIMIT 1`, "boolean"},
	} {
		t.Run(tt.what, func(t *testing.T) {
			if got := pgTypeOf(t, conn, tt.sql); got != tt.want {
				t.Errorf("pg_typeof = %q, this package claims %q", got, tt.want)
			}
		})
	}
}

// jsonIDs runs a composed query over users returning their ids.
func jsonIDs(t *testing.T, db *gendemo.DB, p orm.Predicate[orm.Composed]) []int64 {
	t.Helper()
	shape := orm.Project1(orm.Of(gendemo.Users.ID), func(v int64) int64 { return v })
	got, err := orm.Compose(db.Executor(), shape).
		From(gendemo.Users.Source()).
		Where(p).
		OrderBy(orm.Of(gendemo.Users.ID).Asc()).
		All(t.Context())
	if err != nil {
		t.Fatalf("All: %v", err)
	}
	if got == nil {
		got = []int64{}
	}
	return got
}

func strptr(s string) *string { return &s }
func int32ptr(v int32) *int32 { return &v }
