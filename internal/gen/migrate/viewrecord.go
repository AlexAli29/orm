package migrate

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// Recording what a server made of a definition.
//
// This is the mechanism that lets a read-only check detect that somebody
// altered a view by hand, and it exists because the obvious approaches are all
// forbidden or wrong.
//
// A project's own SQL cannot be compared against pg_get_viewdef: PostgreSQL
// returns a reconstruction of the parsed query, so the two differ even when
// nothing has changed. Canonicalising the project's SQL would mean parsing it,
// and a SQL parser that is approximately right about PostgreSQL is worse than
// none. Asking the server to canonicalise it means creating the view, and orm
// check is read-only — including transient DDL that rolls back, which is still
// DDL and still needs privileges a check should not have.
//
// What is left is to ask the server once, at the only moment DDL is legitimate:
// when the migration applies. CREATE VIEW hands PostgreSQL the definition; the
// migrator reads back what PostgreSQL made of it and records that here, in the
// database it applied to. A later check reads the recording and reads
// pg_get_viewdef, and compares those two — both from one server, one deparser,
// so formatting and comments are already gone and a changed predicate is not.
//
// The recording is per-database and is never committed. It is deparsed text,
// which differs between PostgreSQL majors, so putting it in orm.lock would make
// an identical project produce different bytes on 15 and on 16. The lock holds
// the portable source identity instead.

// ViewStateTable is where the canonical definition of each managed view is
// recorded, in the database it was applied to.
const ViewStateTable = "orm_schema_views"

func viewStateTable() string { return qualified(HistorySchema, ViewStateTable) }

// ViewState is what one server made of one view's definition.
type ViewState struct {
	Schema string
	Name   string
	// Kind is "view" or "materialized view".
	Kind string
	// SourceIdentity is the portable fingerprint of the declaration that was
	// applied. It is here so that a check can tell "the database was edited"
	// from "the declaration moved on and nobody migrated".
	SourceIdentity string
	// Canonical is what this server deparsed the definition to at apply time.
	// It is only ever compared against another reading from this same server.
	Canonical string
}

// EnsureViewState creates the recording table.
//
// It runs where EnsureHistory runs, under the same advisory lock and at the
// same moment: this is migration, which is the one command entitled to create
// anything.
func (m *Migrator) EnsureViewState(ctx context.Context) error {
	_, err := m.conn.Exec(ctx, `CREATE TABLE IF NOT EXISTS `+viewStateTable()+` (
		schema_name     text NOT NULL,
		relation_name   text NOT NULL,
		kind            text NOT NULL,
		source_identity text NOT NULL,
		canonical       text NOT NULL,
		recorded_at     timestamptz NOT NULL DEFAULT now(),
		PRIMARY KEY (schema_name, relation_name)
	)`)
	if err != nil {
		return fmt.Errorf("creating the view state table: %w", err)
	}
	return nil
}

// RecordView reads back what the server made of a view and records it.
//
// It is called immediately after the CREATE, inside the migration's own
// transaction, so a migration that fails afterwards leaves no recording either:
// the record and the relation it describes are applied or abandoned together.
func RecordView(ctx context.Context, ex ViewWriter, schemaName, name, kind, sourceIdentity string) error {
	var canonical string
	err := ex.QueryRow(ctx, `SELECT pg_get_viewdef($1::regclass, true)`,
		pgx.Identifier{schemaName, name}.Sanitize()).Scan(&canonical)
	if err != nil {
		return fmt.Errorf("reading back the definition of %s.%s: %w", schemaName, name, err)
	}
	_, err = ex.Exec(ctx, `INSERT INTO `+viewStateTable()+`
		(schema_name, relation_name, kind, source_identity, canonical)
		VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (schema_name, relation_name) DO UPDATE SET
			kind = EXCLUDED.kind,
			source_identity = EXCLUDED.source_identity,
			canonical = EXCLUDED.canonical,
			recorded_at = now()`,
		schemaName, name, kind, sourceIdentity, canonical)
	if err != nil {
		return fmt.Errorf("recording the definition of %s.%s: %w", schemaName, name, err)
	}
	return nil
}

// ForgetView removes a recording, for a view a migration drops.
func ForgetView(ctx context.Context, ex ViewWriter, schemaName, name string) error {
	_, err := ex.Exec(ctx, `DELETE FROM `+viewStateTable()+
		` WHERE schema_name = $1 AND relation_name = $2`, schemaName, name)
	if err != nil {
		return fmt.Errorf("forgetting the definition of %s.%s: %w", schemaName, name, err)
	}
	return nil
}

// ReadViewState reads every recording, for a check.
//
// It reads and nothing else. A database with no recording table has had no
// managed view applied to it, which is a fact rather than a failure — and
// creating the table to discover that would make a read-only command write.
func ReadViewState(ctx context.Context, q ViewReader) (map[string]ViewState, error) {
	var present *string
	if err := q.QueryRow(ctx, `SELECT to_regclass($1)::text`,
		HistorySchema+"."+ViewStateTable).Scan(&present); err != nil {
		return nil, fmt.Errorf("looking for the view state table: %w", err)
	}
	if present == nil {
		return nil, nil
	}
	rows, err := q.Query(ctx, `SELECT schema_name, relation_name, kind, source_identity, canonical
		FROM `+viewStateTable())
	if err != nil {
		return nil, fmt.Errorf("reading the recorded view definitions: %w", err)
	}
	defer rows.Close()
	out := map[string]ViewState{}
	for rows.Next() {
		var v ViewState
		if err := rows.Scan(&v.Schema, &v.Name, &v.Kind, &v.SourceIdentity, &v.Canonical); err != nil {
			return nil, err
		}
		out[v.Schema+"."+v.Name] = v
	}
	return out, rows.Err()
}

// ViewReader is the read half this file needs: what a check is given.
//
// It has no Exec, and that is the point. A check cannot record, create or drop
// anything through this interface because the interface offers nothing that
// could — the read-only guarantee is in the type rather than in a rule somebody
// has to remember.
type ViewReader interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// ViewWriter is the write half, which only a migration is given.
type ViewWriter interface {
	ViewReader
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}
