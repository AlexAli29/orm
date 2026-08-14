// Package managed holds the services the managed workflow is built from.
//
// Three states matter, and keeping them apart is the whole point:
//
//	desired    what the Go declarations ask for
//	migration  what the migrations, applied in order, add up to
//	actual     what the database currently has
//
// Every question a managed project asks is a comparison of two of them, and
// each pair means something different. Desired against migration says whether a
// migration needs writing. Migration against actual says whether one needs
// running — or whether somebody changed the database behind the migrations'
// back. Collapsing them into one "schema mismatch" would answer none of those.
package managed

import (
	"context"
	"encoding/json"
	"fmt"
	"io"

	"github.com/AlexAli29/orm/internal/gen/migrate"
	"github.com/AlexAli29/orm/internal/gen/pgintro"
	"github.com/AlexAli29/orm/internal/gen/schema"
	"github.com/jackc/pgx/v5"
)

// Inspect reads a live database into the canonical schema model.
//
// It is read-only and it is the same catalog pass the migration engine's
// round-trip test uses, so what it reports is what a migration would be
// compared against. The migration history table is excluded: it is the engine's
// own bookkeeping and no declaration describes it.
func Inspect(ctx context.Context, conn *pgx.Conn, searchPath []string) (*schema.Schema, error) {
	s, err := pgintro.Canonical(ctx, conn, searchPath)
	if err != nil {
		return nil, err
	}
	return WithoutHistory(s), nil
}

// WithoutHistory returns a copy with the engine's own bookkeeping removed.
//
// Two tables, and both for the same reason: the migration engine creates them,
// no declaration describes them, and a drift check comparing the database
// against the declarations would report each one as an unexplained table
// forever. They are the engine's record of what it did, not part of anybody's
// schema.
func WithoutHistory(s *schema.Schema) *schema.Schema {
	out := s.Clone()
	tables := out.Tables[:0]
	for _, t := range out.Tables {
		if t.Schema != migrate.HistorySchema {
			tables = append(tables, t)
			continue
		}
		if t.Name == migrate.HistoryTable || t.Name == migrate.ViewStateTable {
			continue
		}
		tables = append(tables, t)
	}
	out.Tables = tables
	return out
}

// Render writes a canonical schema as indented JSON.
//
// The output is deterministic: the schema is normalised first, so two runs
// against one database produce the same bytes and a schema can be committed and
// diffed like anything else.
func Render(w io.Writer, s *schema.Schema) error {
	out := s.Clone()
	out.Normalize()
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(out)
}

// Baseline builds the migration that creates an existing schema.
//
// It is how a database that already exists enters the managed workflow. The
// migration is a real one — it holds the operations that would create the
// schema from nothing, so a fresh database can be built from the history alone
// — but on the database it was taken from it is recorded as applied without
// running, because the tables are already there.
func Baseline(id string, s *schema.Schema) (*migrate.Migration, error) {
	d, err := migrate.Compute(&schema.Schema{}, s, migrate.Options{})
	if err != nil {
		return nil, fmt.Errorf("building the baseline: %w", err)
	}
	if d.Empty() {
		return nil, fmt.Errorf("building the baseline: the schema is empty")
	}
	m := &migrate.Migration{ID: id, Operations: d.Operations, Atomic: true}
	// A baseline of a schema containing a concurrently-created index cannot be
	// atomic, for the same reason any other migration containing one cannot.
	for _, op := range d.Operations {
		if !op.Transactional() {
			m.Atomic = false
		}
	}
	return m, nil
}

// State is one of the three schemas, with what it took to obtain it.
type State struct {
	Schema *schema.Schema
	// Err is why the state could not be determined, if it could not.
	Err error
}

// Known reports whether the state was obtained.
func (s State) Known() bool { return s.Err == nil && s.Schema != nil }

// Comparison is the answer to all three questions at once.
//
// It is deliberately not a boolean. A project can simultaneously need a
// migration written and one run, and telling somebody only that "the schema does
// not match" leaves them to work out which — and to guess wrong.
type Comparison struct {
	Desired   State
	Migration State
	Actual    State

	// ModelChanges are the differences between the desired schema and what the
	// migrations describe. Non-empty means a migration needs writing.
	ModelChanges []string
	// PendingMigrations are the migrations recorded as not yet applied. When
	// this is non-empty, a difference between the migration state and the
	// database is expected rather than alarming.
	PendingMigrations []string
	// Drift is the difference between what the applied migrations say the
	// database should have and what it has. It is computed against the state as
	// of the last applied migration, so pending work is never reported as
	// drift.
	Drift []string
}

// NeedsMigration reports whether the models describe something no migration does.
func (c Comparison) NeedsMigration() bool { return len(c.ModelChanges) > 0 }

// NeedsMigrate reports whether migrations exist that have not been applied.
func (c Comparison) NeedsMigrate() bool { return len(c.PendingMigrations) > 0 }

// HasDrift reports whether the database differs from what its applied history
// says it should be.
func (c Comparison) HasDrift() bool { return len(c.Drift) > 0 }

// CompareInput is what a three-way comparison needs.
type CompareInput struct {
	// Desired is the schema the declarations ask for, and may be nil in
	// database mode where nothing declares one.
	Desired *schema.Schema
	// Set is the migration history.
	Set *migrate.Set
	// Applied names the migrations the database records as applied, in order.
	Applied []migrate.Applied
	// Actual is the live schema, and may be nil when no database was reachable.
	Actual *schema.Schema
}

// Compare answers all three questions.
//
// Drift is computed against the state as of the last *applied* migration rather
// than the latest one, which is what separates "somebody changed the database"
// from "somebody has not run migrate yet". Reporting a pending migration as
// drift would train people to ignore drift.
func Compare(in CompareInput) (Comparison, error) {
	var c Comparison
	c.Desired = State{Schema: in.Desired}
	c.Actual = State{Schema: in.Actual}

	if in.Set == nil {
		return c, fmt.Errorf("comparing states: no migrations were given")
	}
	migrationState, err := in.Set.State()
	if err != nil {
		c.Migration = State{Err: err}
		return c, err
	}
	c.Migration = State{Schema: migrationState}

	if in.Desired != nil {
		c.ModelChanges = schema.Diff(migrationState, in.Desired)
	}

	applied := make(map[string]bool, len(in.Applied))
	for _, a := range in.Applied {
		applied[a.ID] = true
	}
	lastApplied := ""
	for _, m := range in.Set.Migrations() {
		if !applied[m.ID] {
			c.PendingMigrations = append(c.PendingMigrations, m.ID)
			continue
		}
		lastApplied = m.ID
	}

	if in.Actual == nil {
		return c, nil
	}
	// With nothing applied, the database should be empty of anything the
	// migrations describe; with something applied, it should be that state.
	expected := &schema.Schema{}
	if lastApplied != "" {
		expected, err = in.Set.StateAt(lastApplied)
		if err != nil {
			return c, err
		}
	}
	c.Drift = schema.Diff(expected, in.Actual)
	return c, nil
}

// Describe renders a comparison for a person, deterministically.
func (c Comparison) Describe() string {
	var b []byte
	add := func(format string, args ...any) { b = fmt.Appendf(b, format, args...) }

	add("Model state:\n")
	switch {
	case !c.Desired.Known():
		add("    not declared (database mode)\n")
	case c.NeedsMigration():
		add("    %d change(s) the migrations do not describe:\n", len(c.ModelChanges))
		for _, d := range c.ModelChanges {
			add("        %s\n", d)
		}
	default:
		add("    matches the latest migration\n")
	}

	add("\nDatabase state:\n")
	switch {
	case !c.Actual.Known():
		add("    not reachable\n")
	case c.NeedsMigrate():
		add("    %d migration(s) pending: %v\n", len(c.PendingMigrations), c.PendingMigrations)
	case c.HasDrift():
		add("    the database differs from what its applied migrations describe:\n")
		for _, d := range c.Drift {
			add("        %s\n", d)
		}
	default:
		add("    migrated\n")
	}
	return string(b)
}
