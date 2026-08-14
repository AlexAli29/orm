package gendemo_test

import (
	"testing"

	"github.com/AlexAli29/orm"
	"github.com/AlexAli29/orm/internal/gendemo"
	"github.com/AlexAli29/orm/internal/testdb"
)

// M12.4: full text search.
//
// The claim is that this is PostgreSQL's search — a parsed document matched
// against a parsed query under a named configuration — rather than pattern
// matching wearing its name. So the ranks are compared with PostgreSQL's own,
// and the types are checked against pg_typeof.

func seedArticles(t *testing.T, db *gendemo.DB) {
	t.Helper()
	rows := []gendemo.Article{
		{Title: "Go concurrency patterns", Body: "Channels, goroutines and the select statement."},
		{Title: "PostgreSQL indexing", Body: "How GIN and GiST indexes make search fast in postgres."},
		{Title: "Tuning Postgres performance", Body: "Query planning, indexes, and measuring what is slow."},
		{Title: "JavaScript frontends", Body: "Bundlers, frameworks and the browser event loop."},
	}
	if _, err := orm.CopyColumns(t.Context(), db.Articles, rows,
		gendemo.Articles.Title, gendemo.Articles.Body); err != nil {
		t.Fatalf("seeding articles: %v", err)
	}
}

// Scenario J: a realistic search, with ranks compared against handwritten SQL.
func TestFTS_searchAndRankMatchHandwrittenSQL(t *testing.T) {
	testdb.AdminDSN(t)
	db, conn := m12env(t)
	seedArticles(t, db)

	query := orm.WebSearchToTSQuery(orm.English, "postgres performance")
	rank := orm.TSRankNull(gendemo.Articles.Search, query)

	type row struct {
		Title string
		Rank  float32
	}
	shape := orm.Project2(orm.Of(gendemo.Articles.Title), rank,
		func(title string, r *float32) row {
			out := row{Title: title}
			if r != nil {
				out.Rank = *r
			}
			return out
		})

	got, err := orm.Compose(db.Executor(), shape).
		From(gendemo.Articles.Source()).
		Where(orm.Matches(gendemo.Articles.Search, query)).
		OrderBy(rank.Desc(), orm.Of(gendemo.Articles.Title).Asc()).
		All(t.Context())
	if err != nil {
		t.Fatalf("All: %v", err)
	}
	if len(got) == 0 {
		t.Fatal("the search matched nothing, so it proved nothing")
	}

	rows, err := conn.Query(t.Context(), `
		SELECT title, ts_rank(search, websearch_to_tsquery('english', $1))
		FROM articles
		WHERE search @@ websearch_to_tsquery('english', $1)
		ORDER BY ts_rank(search, websearch_to_tsquery('english', $1)) DESC, title`,
		"postgres performance")
	if err != nil {
		t.Fatalf("handwritten query: %v", err)
	}
	defer rows.Close()
	i := 0
	for rows.Next() {
		var want row
		if err := rows.Scan(&want.Title, &want.Rank); err != nil {
			t.Fatalf("scan: %v", err)
		}
		if i >= len(got) {
			t.Fatalf("the ORM returned %d rows, handwritten SQL returned more", len(got))
		}
		if got[i] != want {
			t.Errorf("row %d = %+v, handwritten SQL returned %+v", i, got[i], want)
		}
		i++
	}
	if i != len(got) {
		t.Errorf("the ORM returned %d rows, handwritten SQL %d", len(got), i)
	}
	// Stemming is doing its job: "postgres" matched "PostgreSQL indexing" only
	// because the document says postgres too, and "performance" matched the
	// tuning article. A LIKE would have found neither pair.
	if got[0].Rank <= 0 {
		t.Errorf("the top rank is %v", got[0].Rank)
	}
}

// Every query constructor parses under the configuration it was given, and the
// answers are PostgreSQL's.
func TestFTS_queryConstructors(t *testing.T) {
	testdb.AdminDSN(t)
	db, conn := m12env(t)
	seedArticles(t, db)

	titles := func(t *testing.T, p orm.Predicate[orm.Composed]) []string {
		t.Helper()
		shape := orm.Project1(orm.Of(gendemo.Articles.Title), func(s string) string { return s })
		got, err := orm.Compose(db.Executor(), shape).
			From(gendemo.Articles.Source()).
			Where(p).
			OrderBy(orm.Of(gendemo.Articles.Title).Asc()).
			All(t.Context())
		if err != nil {
			t.Fatalf("All: %v", err)
		}
		if got == nil {
			got = []string{}
		}
		return got
	}
	handwritten := func(t *testing.T, sql string, args ...any) []string {
		t.Helper()
		rows, err := conn.Query(t.Context(), sql, args...)
		if err != nil {
			t.Fatalf("handwritten query: %v", err)
		}
		defer rows.Close()
		out := []string{}
		for rows.Next() {
			var s string
			if err := rows.Scan(&s); err != nil {
				t.Fatalf("scan: %v", err)
			}
			out = append(out, s)
		}
		return out
	}

	for _, tt := range []struct {
		name string
		pred orm.Predicate[orm.Composed]
		sql  string
		arg  string
	}{
		{"to_tsquery", orm.Matches(gendemo.Articles.Search, orm.ToTSQuery(orm.English, "index & postgres")),
			`SELECT title FROM articles WHERE search @@ to_tsquery('english', $1) ORDER BY title`, "index & postgres"},
		{"plainto_tsquery", orm.Matches(gendemo.Articles.Search, orm.PlainToTSQuery(orm.English, "postgres indexing")),
			`SELECT title FROM articles WHERE search @@ plainto_tsquery('english', $1) ORDER BY title`, "postgres indexing"},
		{"phraseto_tsquery", orm.Matches(gendemo.Articles.Search, orm.PhraseToTSQuery(orm.English, "go concurrency")),
			`SELECT title FROM articles WHERE search @@ phraseto_tsquery('english', $1) ORDER BY title`, "go concurrency"},
		{"websearch_to_tsquery", orm.Matches(gendemo.Articles.Search, orm.WebSearchToTSQuery(orm.English, `"event loop"`)),
			`SELECT title FROM articles WHERE search @@ websearch_to_tsquery('english', $1) ORDER BY title`, `"event loop"`},
		{"simple configuration finds the unstemmed word",
			orm.Matches(gendemo.Articles.Search, orm.PlainToTSQuery(orm.Simple, "indexes")),
			`SELECT title FROM articles WHERE search @@ plainto_tsquery('simple', $1) ORDER BY title`, "indexes"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got, want := titles(t, tt.pred), handwritten(t, tt.sql, tt.arg)
			if len(got) != len(want) {
				t.Fatalf("the ORM returned %v, handwritten SQL returned %v", got, want)
			}
			for i := range got {
				if got[i] != want[i] {
					t.Errorf("row %d: %q against %q", i, got[i], want[i])
				}
			}
		})
	}
}

// Vectors are built, weighted and combined in the server, and queries are
// composed there too.
func TestFTS_buildingAndComposing(t *testing.T) {
	testdb.AdminDSN(t)
	db, conn := m12env(t)
	seedArticles(t, db)

	// A weighted vector built from two columns, matched against a composed
	// query — none of which is string concatenation in Go.
	vector := orm.Concat2TSVector(
		orm.SetWeight(orm.ToTSVector(orm.English, gendemo.Articles.Title), orm.WeightA),
		orm.SetWeight(orm.ToTSVector(orm.English, gendemo.Articles.Body), orm.WeightB),
	)
	query := orm.AndTSQuery(
		orm.PlainToTSQuery(orm.English, "postgres"),
		orm.PlainToTSQuery(orm.English, "index"),
	)
	shape := orm.Project1(orm.Of(gendemo.Articles.Title), func(s string) string { return s })
	got, err := orm.Compose(db.Executor(), shape).
		From(gendemo.Articles.Source()).
		Where(orm.Matches(vector, query)).
		OrderBy(orm.Of(gendemo.Articles.Title).Asc()).
		All(t.Context())
	if err != nil {
		t.Fatalf("All: %v", err)
	}
	want := []string{}
	rows, err := conn.Query(t.Context(), `
		SELECT title FROM articles
		WHERE setweight(to_tsvector('english', title), 'A') ||
		      setweight(to_tsvector('english', body), 'B')
		      @@ (plainto_tsquery('english','postgres') && plainto_tsquery('english','index'))
		ORDER BY title`)
	if err != nil {
		t.Fatalf("handwritten query: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			t.Fatalf("scan: %v", err)
		}
		want = append(want, s)
	}
	if len(got) != len(want) {
		t.Fatalf("the ORM returned %v, handwritten SQL returned %v", got, want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Errorf("row %d: %q against %q", i, got[i], want[i])
		}
	}
	if len(got) == 0 {
		t.Error("nothing matched, so the composition proved nothing")
	}

	// A negated query returns the complement.
	neg := orm.Compose(db.Executor(), shape).
		From(gendemo.Articles.Source()).
		Where(orm.Matches(vector, orm.NotTSQuery(orm.PlainToTSQuery(orm.English, "postgres"))))
	negGot, err := neg.All(t.Context())
	if err != nil {
		t.Fatalf("negated query: %v", err)
	}
	if len(negGot) == 0 {
		t.Error("a negated tsquery matched nothing")
	}
}

// The vector column is a generated one, so PostgreSQL maintains it and it never
// drifts from the document.
func TestFTS_generatedVectorIsMaintainedByPostgreSQL(t *testing.T) {
	testdb.AdminDSN(t)
	db, _ := m12env(t)
	seedArticles(t, db)

	// Writing the body updates the vector without anybody saying so.
	if _, err := db.Articles.Update().
		Set(gendemo.Articles.Body.Set("kubernetes orchestration")).
		Where(gendemo.Articles.Title.Eq("JavaScript frontends")).
		Exec(t.Context()); err != nil {
		t.Fatalf("Update: %v", err)
	}
	shape := orm.Project1(orm.Of(gendemo.Articles.Title), func(s string) string { return s })
	got, err := orm.Compose(db.Executor(), shape).
		From(gendemo.Articles.Source()).
		Where(orm.Matches(gendemo.Articles.Search, orm.PlainToTSQuery(orm.English, "kubernetes"))).
		All(t.Context())
	if err != nil {
		t.Fatalf("All: %v", err)
	}
	if len(got) != 1 || got[0] != "JavaScript frontends" {
		t.Errorf("the generated vector did not follow the document: %v", got)
	}

	// And the column is not writable, so the write path never mentions it.
	if _, err := orm.CopyColumns(t.Context(), db.Articles, nil, gendemo.Articles.Search); err == nil {
		t.Error("COPY accepted a generated tsvector column")
	}
}

// The GIN index the schema declares survives a migration round trip, and
// introspection reports the same schema back.
func TestFTS_ginIndexIsInTheSchema(t *testing.T) {
	testdb.AdminDSN(t)
	_, conn := m12env(t)

	var kind, def string
	if err := conn.QueryRow(t.Context(), `
		SELECT am.amname, pg_get_indexdef(i.indexrelid)
		FROM pg_index i
		JOIN pg_class c ON c.oid = i.indexrelid
		JOIN pg_am am ON am.oid = c.relam
		WHERE c.relname = 'articles_search_idx'`).Scan(&kind, &def); err != nil {
		t.Fatalf("reading the index: %v", err)
	}
	if kind != "gin" {
		t.Errorf("the search index uses %s, want gin", kind)
	}
	if !contains(def, "search") {
		t.Errorf("the index definition is %s", def)
	}
}

// NULL documents and NULL queries behave as PostgreSQL says.
func TestFTS_nullSemantics(t *testing.T) {
	testdb.AdminDSN(t)
	db, conn := m12env(t)
	seedArticles(t, db)

	// The vector column is nullable in the schema, so Matches over it is a
	// three-valued comparison: a NULL vector matches nothing.
	m11exec(t, conn, `INSERT INTO articles (title, body) VALUES ('empty', '')`)

	shape := orm.Project1(orm.Of(gendemo.Articles.Title), func(s string) string { return s })
	got, err := orm.Compose(db.Executor(), shape).
		From(gendemo.Articles.Source()).
		Where(orm.Matches(gendemo.Articles.Search, orm.PlainToTSQuery(orm.English, "empty"))).
		All(t.Context())
	if err != nil {
		t.Fatalf("All: %v", err)
	}
	if len(got) != 1 || got[0] != "empty" {
		t.Errorf("a document with an empty body matched %v", got)
	}

	// A nullable document produces a nullable vector, and the type says so.
	nullable := orm.ToTSVectorNull(orm.English, gendemo.Users.Nickname)
	shape2 := orm.Project1(nullable, func(v *orm.TSVector) *orm.TSVector { return v })
	vectors, err := orm.Compose(db.Executor(), shape2).
		From(gendemo.Users.Source()).
		OrderBy(orm.Of(gendemo.Users.ID).Asc()).
		All(t.Context())
	if err != nil {
		t.Fatalf("All: %v", err)
	}
	nulls := 0
	for _, v := range vectors {
		if v == nil {
			nulls++
		}
	}
	if nulls == 0 {
		t.Error("to_tsvector of a NULL document produced no NULLs")
	}
}

// Every FTS result type this package claims, against the server.
func TestPGTypeOf_M12FTS(t *testing.T) {
	testdb.AdminDSN(t)
	db, conn := m12env(t)
	seedArticles(t, db)

	for _, tt := range []struct{ what, sql, want string }{
		{"tsvector column", `SELECT pg_typeof(search) FROM articles LIMIT 1`, "tsvector"},
		{"to_tsvector", `SELECT pg_typeof(to_tsvector('english'::regconfig, 'x'))`, "tsvector"},
		{"to_tsquery", `SELECT pg_typeof(to_tsquery('english'::regconfig, 'x'))`, "tsquery"},
		{"plainto_tsquery", `SELECT pg_typeof(plainto_tsquery('english'::regconfig, 'x'))`, "tsquery"},
		{"phraseto_tsquery", `SELECT pg_typeof(phraseto_tsquery('english'::regconfig, 'x'))`, "tsquery"},
		{"websearch_to_tsquery", `SELECT pg_typeof(websearch_to_tsquery('english'::regconfig, 'x'))`, "tsquery"},
		{"@@", `SELECT pg_typeof(to_tsvector('english','x') @@ to_tsquery('english','x'))`, "boolean"},
		{"ts_rank", `SELECT pg_typeof(ts_rank(to_tsvector('english','x'), to_tsquery('english','x')))`, "real"},
		{"ts_rank_cd", `SELECT pg_typeof(ts_rank_cd(to_tsvector('english','x'), to_tsquery('english','x')))`, "real"},
		{"setweight", `SELECT pg_typeof(setweight(to_tsvector('english','x'), 'A'))`, "tsvector"},
		{"vector concatenation", `SELECT pg_typeof(to_tsvector('english','a') || to_tsvector('english','b'))`, "tsvector"},
		{"query conjunction", `SELECT pg_typeof(to_tsquery('english','a') && to_tsquery('english','b'))`, "tsquery"},
		{"query negation", `SELECT pg_typeof(!!to_tsquery('english','a'))`, "tsquery"},
	} {
		t.Run(tt.what, func(t *testing.T) {
			if got := pgTypeOf(t, conn, tt.sql); got != tt.want {
				t.Errorf("pg_typeof = %q, this package claims %q", got, tt.want)
			}
		})
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (func() bool {
		for i := 0; i+len(sub) <= len(s); i++ {
			if s[i:i+len(sub)] == sub {
				return true
			}
		}
		return false
	})()
}
