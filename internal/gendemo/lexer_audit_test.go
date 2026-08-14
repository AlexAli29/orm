package gendemo_test

import (
	"context"
	"strings"
	"testing"

	"github.com/AlexAli29/orm/internal/expr"
	"github.com/AlexAli29/orm/internal/testdb"
	"github.com/jackc/pgx/v5"
)

// The placeholder lexer, checked against the authority.
//
// The lexer exists to decide which $n in a raw fragment PostgreSQL would read
// as a parameter. Every table test for it encodes somebody's belief about that;
// this one asks the server. A fragment goes to PREPARE, PostgreSQL reports how
// many parameters it found, and the lexer has to agree — because a fragment
// where they disagree is one whose arguments get renumbered into the wrong
// positions, which is a wrong query rather than a failed one.
func TestAudit_lexerAgreesWithPostgres(t *testing.T) {
	testdb.AdminDSN(t)
	dsn := testdb.Create(t, "CREATE TABLE t (a text, b int, c bool);")
	conn, err := pgx.Connect(t.Context(), dsn)
	if err != nil {
		t.Fatalf("connecting: %v", err)
	}
	defer func() { _ = conn.Close(context.WithoutCancel(t.Context())) }()

	// Each fragment is a WHERE condition, which is where a raw fragment
	// actually lands. Every parameter from $1 to the highest is referenced,
	// because PostgreSQL cannot infer the type of one nothing uses — which is
	// the same reason ValidatePlaceholders refuses an unreferenced argument.
	fragments := []string{
		`a = $1`,
		`a = $1 AND b = $2`,
		`a = $2 AND b = $1`,
		`a = $1 AND b = $2 AND c = $3`,
		`a = '$1'`,
		`a = '$1' AND b = $1`,
		`a = E'\\$1'`,
		`a = E'\\$1' AND b = $1`,
		`a = $$ $1 $$`,
		`a = $$ $1 $$ AND b = $1`,
		`a = $tag$ $1 $tag$`,
		`a = $tag$ $1 $tag$ AND b = $1`,
		"a = 'x' -- $1\n AND b = $1",
		`a = 'x' /* $1 */ AND b = $1`,
		"a = 'x' /*\n nested /* $2 */\n $3\n*/ AND b = $1",
		`b = $1::int4`,
		`b = ANY($1::int4[])`,
		`a LIKE $1 || '%'`,
		`a = 'it''s $1' AND b = $1`,
		`c = ($1::bool)`,
		`a = $1 AND a = $1`,
		`(SELECT count(*) FROM t x WHERE x.a = $1) > 0`,
		`a = 'é$1é' AND b = $1`,
		`a = $1 AND b = 2 AND c = true`,
	}

	for i, frag := range fragments {
		sql := "SELECT 1 FROM t WHERE " + frag

		refs, err := expr.ScanPlaceholders(frag)
		if err != nil {
			t.Errorf("%q: ScanPlaceholders: %v", frag, err)
			continue
		}
		distinct := map[int]bool{}
		highest := 0
		for _, r := range refs {
			distinct[r.Index] = true
			if r.Index > highest {
				highest = r.Index
			}
			// The span has to cover exactly the reference, or renumbering would
			// splice over the wrong bytes.
			if got := frag[r.Start:r.End]; !strings.HasPrefix(got, "$") {
				t.Errorf("%q: span %d:%d is %q", frag, r.Start, r.End, got)
			}
		}

		sd, err := conn.Prepare(t.Context(), "audit"+string(rune('a'+i)), sql)
		if err != nil {
			t.Fatalf("%q: PostgreSQL refused the statement: %v", sql, err)
		}
		// PostgreSQL reports one OID per parameter position, from $1 to the
		// highest referenced. That is the number the lexer has to reach.
		if len(sd.ParamOIDs) != highest {
			t.Errorf("%q: PostgreSQL found %d parameters, the lexer found references up to $%d (%d distinct)",
				frag, len(sd.ParamOIDs), highest, len(distinct))
		}
	}
}

// The other half: a fragment whose placeholders PostgreSQL would not read as
// parameters must not be renumbered, and the renumbered fragment must still be
// a statement PostgreSQL accepts.
func TestAudit_renumberingPreservesMeaning(t *testing.T) {
	testdb.AdminDSN(t)
	dsn := testdb.Create(t, "CREATE TABLE t (a text, b int);\nINSERT INTO t VALUES ('$1', 7), ('x', 8);")
	conn, err := pgx.Connect(t.Context(), dsn)
	if err != nil {
		t.Fatalf("connecting: %v", err)
	}
	defer func() { _ = conn.Close(context.WithoutCancel(t.Context())) }()

	// The literal '$1' is data, not a parameter. Renumbering the real
	// parameter must leave it alone — and the row whose column holds the text
	// "$1" is what proves it.
	frag := `a = '$1' AND b = $1`
	refs, err := expr.ScanPlaceholders(frag)
	if err != nil {
		t.Fatalf("ScanPlaceholders: %v", err)
	}
	if len(refs) != 1 {
		t.Fatalf("found %d references in %q, want 1", len(refs), frag)
	}
	// Shift the fragment's arguments by one, as if it had joined a statement
	// that already had a parameter of its own.
	shifted := expr.RewritePlaceholders(frag, refs, func(i int) string { return "$" + string(rune('0'+i+1)) })
	if shifted != `a = '$1' AND b = $2` {
		t.Fatalf("renumbered to %q", shifted)
	}

	var count int
	if err := conn.QueryRow(t.Context(),
		"SELECT count(*) FROM t WHERE a <> $1 AND ("+shifted+")", "never", 7).Scan(&count); err != nil {
		t.Fatalf("running the renumbered fragment: %v", err)
	}
	// The row that matches is the one whose column literally holds "$1", which
	// only happens if the text inside the quotes was left alone.
	if count != 1 {
		t.Errorf("the renumbered fragment matched %d rows, want 1", count)
	}
}
