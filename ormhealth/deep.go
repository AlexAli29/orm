package ormhealth

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/AlexAli29/orm/gen/diag"
	"github.com/AlexAli29/orm/internal/gen"
	"github.com/AlexAli29/orm/internal/gen/config"
	"github.com/AlexAli29/orm/internal/gen/migrate"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// pgxRows is the part of pgx.Rows this package reads, named so that [Querier]
// matches the ORM's own Executor without importing more of pgx than it uses.
type pgxRows = pgx.Rows

// DeepOption asks for one of the checks that costs something.
//
// Nothing is on by default beyond connectivity and the version, because a caller
// who wanted a schema reconciliation would say so, and one who did not should
// not pay for it. That is the same reason [Quick] exists.
type DeepOption func(*deepConfig)

type deepConfig struct {
	extensions []string
	configPath string
	migrateDir string
}

// WithExtensions checks that the named extensions are installed.
//
// It reports what it finds and installs nothing. CREATE EXTENSION is a schema
// change requiring privileges a service should not have, and a health check that
// performed one would be a health check that could not be given to a probe.
func WithExtensions(names ...string) DeepOption {
	return func(c *deepConfig) { c.extensions = append(c.extensions, names...) }
}

// WithSchemaCheck reconciles the Go types against the live database, using the
// project's own configuration file.
//
// It runs the reconciliation `orm check` runs — the same path, the same
// diagnostics — and reads the catalog to do it. It is the most expensive thing
// in this package and is why [Deep] is not what a probe calls.
//
// It reports drift. It does not migrate.
func WithSchemaCheck(configPath string) DeepOption {
	return func(c *deepConfig) { c.configPath = configPath }
}

// WithMigrationState reads how far the database has been migrated.
//
// It compares the migrations on disk against the history table the engine
// maintains, and reports what is outstanding. It applies nothing: a service that
// migrated its own database on a health check would migrate it once per replica,
// concurrently, during a rollout.
func WithMigrationState(dir string) DeepOption {
	return func(c *deepConfig) { c.migrateDir = dir }
}

// Deep runs the operator's checks.
//
// Connectivity and the server version always run, because they are one round
// trip each and nothing else is interpretable without them. Everything else is
// opt-in through the options above.
//
// Every check is a read. Nothing in this function migrates, installs, tunes or
// resizes anything.
func Deep(ctx context.Context, q Querier, opts ...DeepOption) DeepReport {
	var cfg deepConfig
	for _, o := range opts {
		if o != nil {
			o(&cfg)
		}
	}

	r := DeepReport{Connectivity: Quick(ctx, q)}
	if !r.Connectivity.OK() {
		// Nothing else can be asked of a database that did not answer, and
		// asking would turn one failure into several timeouts.
		r.Status = StatusDown
		return r
	}
	r.Status = StatusUp

	if v, num, err := serverVersion(ctx, q); err == nil {
		r.Version, r.VersionNum = v, num
	}

	if p := poolOf(q); p != nil {
		r.Pool = statsOf(p)
	}

	for _, name := range cfg.extensions {
		ext, err := extensionState(ctx, q, name)
		if err != nil {
			r.Extensions = append(r.Extensions, Extension{Name: name})
			r.Status = worst(r.Status, StatusDegraded)
			continue
		}
		r.Extensions = append(r.Extensions, ext)
		if !ext.Installed {
			r.Status = worst(r.Status, StatusDegraded)
		}
	}

	if cfg.configPath != "" {
		s := schemaState(ctx, q, cfg.configPath)
		r.Schema = &s
		r.Status = worst(r.Status, s.Status)
	}

	if cfg.migrateDir != "" {
		m := migrationState(ctx, q, cfg.migrateDir)
		r.Migrations = &m
		r.Status = worst(r.Status, m.Status)
	}
	return r
}

// worst keeps the most serious status seen, with unknown never masking a real
// problem and never inventing one: a check that could not run says so without
// making the database look broken.
func worst(a, b Status) Status {
	rank := map[Status]int{StatusUp: 0, StatusUnknown: 1, StatusDegraded: 2, StatusDown: 3}
	if rank[b] > rank[a] {
		return b
	}
	return a
}

func serverVersion(ctx context.Context, q Querier) (string, int, error) {
	rows, err := q.Query(ctx, `SELECT current_setting('server_version'), current_setting('server_version_num')::int`)
	if err != nil {
		return "", 0, err
	}
	defer rows.Close()
	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return "", 0, err
		}
		return "", 0, fmt.Errorf("ormhealth: the server reported no version")
	}
	var version string
	var num int
	if err := rows.Scan(&version, &num); err != nil {
		return "", 0, err
	}
	rows.Close()
	return version, num, rows.Err()
}

// extensionState asks the catalog whether an extension is installed.
//
// It reads pg_extension, which says what is installed in this database, rather
// than pg_available_extensions, which says what could be. The question an
// operator is asking is the former.
func extensionState(ctx context.Context, q Querier, name string) (Extension, error) {
	rows, err := q.Query(ctx, `SELECT extversion FROM pg_extension WHERE extname = $1`, name)
	if err != nil {
		return Extension{Name: name}, err
	}
	defer rows.Close()
	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return Extension{Name: name}, err
		}
		return Extension{Name: name, Installed: false}, nil
	}
	var version string
	if err := rows.Scan(&version); err != nil {
		return Extension{Name: name}, err
	}
	rows.Close()
	return Extension{Name: name, Installed: true, Version: version}, rows.Err()
}

// schemaState runs the project's own reconciliation, after checking it is about
// the database the caller handed over.
//
// The reconciliation connects for itself, using the DSN in the configuration
// file — it has to, because it reads the catalog through the generator's own
// pipeline. That leaves a gap worth closing loudly: an operator can hand Deep a
// connection to one database while the configuration names another, and every
// other line of the report would be about the first while the schema line was
// about the second. Nothing in the output would look wrong.
//
// So the two are compared first, and a mismatch is reported as unknown with both
// names in it rather than reconciled anyway. It is the realistic mistake —
// pointing a check at staging with production's configuration on the path — and
// it is the one a health report must not answer confidently.
func schemaState(ctx context.Context, q Querier, configPath string) SchemaState {
	cfg, err := config.Load(configPath)
	if err != nil {
		return SchemaState{Status: StatusUnknown, Err: fmt.Errorf("loading %s: %w", configPath, err)}
	}
	if err := sameDatabase(ctx, q, cfg.Schema.DSN); err != nil {
		return SchemaState{Status: StatusUnknown, Err: err}
	}
	result, err := gen.Check(ctx, cfg)
	if err != nil {
		return SchemaState{Status: StatusUnknown, Err: err}
	}
	if result.Report == nil || !result.Report.Failed(diag.SeverityError) {
		return SchemaState{Status: StatusUp}
	}

	seen := map[string]bool{}
	var codes []string
	count := 0
	for _, f := range result.Report.Findings() {
		if f.Severity != diag.SeverityError {
			continue
		}
		count++
		// The code, not the message. A code is stable enough to alert on; a
		// message names columns and types and would make every report unique.
		code := string(f.Code)
		if !seen[code] {
			seen[code] = true
			codes = append(codes, code)
		}
	}
	slices.Sort(codes)
	return SchemaState{Status: StatusDegraded, Findings: count, Codes: codes}
}

// sameDatabase reports whether the configuration's DSN names the database the
// querier is connected to.
//
// It compares the database name, and only that.
//
// The name is the part a client and a server agree on. Everything else that
// looks comparable is not: current_setting('port') is the port the server
// listens on inside its own network namespace, which a mapped container or a
// connection pooler makes differ from the one the client dialled, and the host
// in a DSN can be an alias for the address the server reports. A check that
// compared those would reject correct setups, which is worse than the gap it
// closes — an operational check that cries wolf gets switched off.
//
// So this is deliberately not a proof of identity. It catches the realistic
// mistake: pointing a check at one database while the configuration on the path
// names another.
func sameDatabase(ctx context.Context, q Querier, dsn string) error {
	if dsn == "" {
		return nil
	}
	want, err := pgx.ParseConfig(dsn)
	if err != nil {
		return fmt.Errorf("the configuration's DSN could not be parsed, so the schema check cannot confirm it is about this database: %w", err)
	}

	rows, err := q.Query(ctx, `SELECT current_database()`)
	if err != nil {
		return fmt.Errorf("reading which database this is: %w", err)
	}
	defer rows.Close()
	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return err
		}
		return fmt.Errorf("the server did not say which database this is")
	}
	var haveDB string
	if err := rows.Scan(&haveDB); err != nil {
		return err
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}

	if want.Database != "" && want.Database != haveDB {
		return fmt.Errorf("the schema check was not run: this connection is to database %q "+
			"and the configuration names %q, so reconciling would report on a database you did not ask about",
			haveDB, want.Database)
	}
	return nil
}

// migrationState compares the migrations on disk against the engine's history.
func migrationState(ctx context.Context, q Querier, dir string) MigrationState {
	set, err := migrate.NewStore(dir).Load()
	if err != nil {
		return MigrationState{Status: StatusUnknown, Err: fmt.Errorf("loading migrations from %s: %w", dir, err)}
	}

	applied, err := appliedIDs(ctx, q)
	if err != nil {
		// A missing history table is a database nothing has migrated yet, which
		// is a fact rather than an error: everything is pending.
		//
		// Any other failure is not that fact, and must not be reported as
		// though it were. A permission error, a broken connection or a history
		// table this version does not understand would otherwise come back as
		// "nothing has been migrated" — the most alarming answer there is,
		// stated with no error attached, about a database that may be perfectly
		// up to date.
		if !noHistoryTable(err) {
			return MigrationState{Status: StatusUnknown, Err: fmt.Errorf("reading migration history: %w", err)}
		}
		var st MigrationState
		st.Status = StatusDegraded
		for _, m := range set.Migrations() {
			st.Pending++
			st.PendingIDs = append(st.PendingIDs, m.ID)
		}
		if st.Pending == 0 {
			st.Status = StatusUp
		}
		return st
	}

	st := MigrationState{Status: StatusUp, Applied: len(applied)}
	for _, m := range set.Migrations() {
		if !applied[m.ID] {
			st.Pending++
			st.PendingIDs = append(st.PendingIDs, m.ID)
		}
	}
	if st.Pending > 0 {
		st.Status = StatusDegraded
	}
	return st
}

// noHistoryTable reports whether the error is the history table not existing,
// which is the one failure that means something other than "the check failed":
// a database nothing has ever migrated.
func noHistoryTable(err error) bool {
	var pg *pgconn.PgError
	// 42P01 undefined_table, 3F000 invalid_schema_name — the schema the history
	// table lives in may not be there either.
	return errors.As(err, &pg) && (pg.Code == "42P01" || pg.Code == "3F000")
}

// appliedIDs reads the engine's history table.
//
// The column is migration_id, which is what the migration engine writes. This
// reads the engine's own table rather than a copy of its shape, so the identifier
// comes from the engine's constants — but the column name has to be spelled
// here, and spelling it wrongly is silent: the query fails, and a failure to
// read history looks exactly like a database that has never been migrated.
// [TestDeep_migrationStateMatchesTheEngine] is what keeps the two in agreement.
func appliedIDs(ctx context.Context, q Querier) (map[string]bool, error) {
	stmt := fmt.Sprintf(`SELECT migration_id FROM %s.%s`, migrate.HistorySchema, migrate.HistoryTable)
	rows, err := q.Query(ctx, stmt)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out[id] = true
	}
	return out, rows.Err()
}
