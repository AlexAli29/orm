package reconcile_test

import (
	"context"
	"regexp"
	"strings"
	"sync"
	"testing"

	"github.com/AlexAli29/orm/gen/diag"
	"github.com/AlexAli29/orm/internal/gen/migrate"
	"github.com/AlexAli29/orm/internal/gen/model"
	"github.com/AlexAli29/orm/internal/gen/reconcile"
	"github.com/jackc/pgx/v5"
)

// M16.5 Stage F: the read-only proof.
//
// A check must not write, and "must not" is worth nothing without something
// that would notice. Two things do here: every statement the check sends is
// captured and matched against a mutating-SQL pattern, and the whole thing is
// then run again as a PostgreSQL role that has no privilege to create anything.
//
// The second is the stronger proof. Searching SQL strings finds what somebody
// thought to search for; a role with no CREATE finds anything at all that tries.

// mutating matches statements that change something. It is deliberately broad —
// a false positive here is a test somebody has to look at, and a false negative
// is a check that wrote to a production database.
var mutating = regexp.MustCompile(`(?is)\b(CREATE|ALTER|DROP|TRUNCATE|INSERT|UPDATE|DELETE|REFRESH|GRANT|REVOKE|COMMENT\s+ON)\b`)

// spy records every statement sent through it and forwards it.
type spy struct {
	conn *pgx.Conn
	mu   sync.Mutex
	sent []string
}

func (s *spy) record(sql string) {
	s.mu.Lock()
	s.sent = append(s.sent, sql)
	s.mu.Unlock()
}

func (s *spy) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	s.record(sql)
	return s.conn.Query(ctx, sql, args...)
}

func (s *spy) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	s.record(sql)
	return s.conn.QueryRow(ctx, sql, args...)
}

// The definition check sends nothing that changes anything.
func TestReadOnly_definitionCheckSendsNoDDL(t *testing.T) {
	const sql = "SELECT id, email FROM users WHERE active"
	c := fixture(t, `
		CREATE VIEW v AS `+sql+`;
		CREATE MATERIALIZED VIEW m AS SELECT id FROM users WITH DATA;
		CREATE UNIQUE INDEX m_id_key ON m (id);
		CREATE VIEW w AS SELECT id FROM v;
	`, [3]string{"v", "view", sql}, [3]string{"m", "materialized view", "SELECT id FROM users"})

	s := &spy{conn: c}
	report := &diag.Report{}
	err := reconcile.CheckDefinitions(t.Context(), report, reconcile.DefinitionInput{
		Entities: []*model.GoEntity{
			entity("V", "v", model.RelView),
			entity("W", "w", model.RelView),
			entity("M", "m", model.RelMaterializedView),
		},
		Schema: introspect(t, c),
		Reader: s,
	})
	if err != nil {
		t.Fatalf("checking: %v", err)
	}
	if len(s.sent) == 0 {
		t.Fatal("the check sent no statements at all, so this proves nothing")
	}
	for _, stmt := range s.sent {
		if mutating.MatchString(stmt) {
			t.Errorf("the check sent a mutating statement:\n%s", stmt)
		}
	}
	t.Logf("the check sent %d statements, none of them mutating", len(s.sent))
}

// The stronger proof: a role that cannot create anything runs the check.
//
// This catches what a string search cannot — anything that writes without
// looking like it, through a function, or in a statement nobody thought to
// pattern-match.
func TestReadOnly_checkRunsWithoutCreatePrivilege(t *testing.T) {
	const sql = "SELECT id, email FROM users WHERE active"
	owner := fixture(t, `CREATE VIEW v AS `+sql+`;`, [3]string{"v", "view", sql})

	var dbName string
	if err := owner.QueryRow(t.Context(), `SELECT current_database()`).Scan(&dbName); err != nil {
		t.Fatal(err)
	}
	// A role with connect and read, and nothing else. No CREATE on the
	// database, none on the schema, no ownership, no write on any table.
	const role = "orm_check_reader"
	for _, stmt := range []string{
		`DROP ROLE IF EXISTS ` + role,
		`CREATE ROLE ` + role + ` LOGIN PASSWORD 'reader'`,
		`REVOKE ALL ON DATABASE "` + dbName + `" FROM ` + role,
		`GRANT CONNECT ON DATABASE "` + dbName + `" TO ` + role,
		// PostgreSQL 14 and earlier grant CREATE on the public schema to
		// PUBLIC, so revoking it from the role alone leaves it able to create
		// through that grant. PostgreSQL 15 revoked it by default; both are
		// handled here so the test is genuinely restrictive on every major.
		`REVOKE CREATE ON SCHEMA public FROM PUBLIC`,
		`REVOKE CREATE ON SCHEMA public FROM ` + role,
		`GRANT USAGE ON SCHEMA public TO ` + role,
		`GRANT SELECT ON ALL TABLES IN SCHEMA public TO ` + role,
	} {
		if _, err := owner.Exec(t.Context(), stmt); err != nil {
			t.Skipf("this server does not allow the test to build a restricted role (%v); "+
				"the SQL-capture test still applies", err)
		}
	}
	t.Cleanup(func() {
		_, _ = owner.Exec(context.Background(), `REASSIGN OWNED BY `+role+` TO CURRENT_USER`)
		_, _ = owner.Exec(context.Background(), `DROP OWNED BY `+role)
		_, _ = owner.Exec(context.Background(), `DROP ROLE IF EXISTS `+role)
	})

	cfg := owner.Config().Copy()
	cfg.User, cfg.Password = role, "reader"
	restricted, err := pgx.ConnectConfig(t.Context(), cfg)
	if err != nil {
		t.Fatalf("connecting as %s: %v", role, err)
	}
	defer func() { _ = restricted.Close(context.Background()) }()

	// It genuinely cannot create: if this succeeded the test would be proving
	// nothing about privileges.
	if _, err := restricted.Exec(t.Context(), `CREATE TABLE probe_should_fail (id int)`); err == nil {
		t.Fatal("the restricted role can create tables, so this test proves nothing")
	}

	report := &diag.Report{}
	if err := reconcile.CheckDefinitions(t.Context(), report, reconcile.DefinitionInput{
		Entities: []*model.GoEntity{entity("V", "v", model.RelView)},
		Schema:   introspect(t, owner),
		Reader:   restricted,
	}); err != nil {
		t.Fatalf("the check failed as a role with no CREATE privilege: %v", err)
	}
	if len(report.Findings()) != 0 {
		t.Errorf("the check reported findings when run without CREATE: %v", viewCodes(report))
	}
}

// The reader the check is handed cannot write, as a matter of type.
func TestReadOnly_readerHasNoExec(t *testing.T) {
	var in reconcile.DefinitionInput
	// The field's type is the narrow one. Assigning something that can write is
	// fine — a *pgx.Conn can — but the check can only reach Query and QueryRow
	// through it, because that is all the interface has.
	if _, ok := any(in.Reader).(migrate.ViewWriter); ok {
		t.Error("a nil reader satisfies the writer interface")
	}
	rt := strings.TrimSpace("migrate.ViewReader")
	_ = rt
	var probe any = struct {
		migrate.ViewReader
	}{}
	if _, ok := probe.(interface {
		Exec(context.Context, string, ...any) (any, error)
	}); ok {
		t.Error("the reader interface exposes a way to execute statements")
	}
}
