package ormhealth_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/AlexAli29/orm/internal/gendemo"
	"github.com/AlexAli29/orm/internal/testdb"
	"github.com/AlexAli29/orm/ormhealth"
	"github.com/AlexAli29/orm/ormtest"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// M15.2: health and operational introspection.
//
// The claims are that the quick check is cheap enough for a probe, that the deep
// check reports facts without changing anything, and that neither carries a
// credential or a value.

func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := filepath.Abs("..")
	if err != nil {
		t.Fatalf("resolving the repository root: %v", err)
	}
	return dir
}

func schema(t *testing.T) string {
	t.Helper()
	ddl, err := os.ReadFile(filepath.Join(repoRoot(t), "internal", "gendemo", "schema.sql"))
	if err != nil {
		t.Fatalf("reading the fixture schema: %v", err)
	}
	return string(ddl)
}

func pool(t *testing.T, ddl string) *pgxpool.Pool {
	t.Helper()
	testdb.AdminDSN(t)
	dsn := testdb.Create(t, ddl)
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatalf("parsing the DSN: %v", err)
	}
	cfg.MaxConns = 4
	cfg.AfterConnect = gendemo.RegisterTypes
	p, err := pgxpool.NewWithConfig(t.Context(), cfg)
	if err != nil {
		t.Fatalf("opening a pool: %v", err)
	}
	t.Cleanup(p.Close)
	return p
}

// Scenario E: a healthy database answers, and an unavailable one fails promptly
// rather than hanging.
func TestQuick(t *testing.T) {
	t.Run("a healthy database is up", func(t *testing.T) {
		p := pool(t, schema(t))
		r := ormhealth.Quick(t.Context(), p)
		if !r.OK() {
			t.Fatalf("status = %s, err = %v", r.Status, r.Err)
		}
		if r.Latency <= 0 {
			t.Error("no latency was measured")
		}
		// Cheap enough for a probe: one round trip.
		if r.Latency > 2*time.Second {
			t.Errorf("a readiness check took %v", r.Latency)
		}
	})

	t.Run("an unreachable database is down, promptly", func(t *testing.T) {
		// A port nothing listens on, with a deadline a probe would set.
		cfg, err := pgxpool.ParseConfig("postgres://nobody@127.0.0.1:1/none?sslmode=disable&connect_timeout=1")
		if err != nil {
			t.Fatalf("parsing: %v", err)
		}
		p, err := pgxpool.NewWithConfig(t.Context(), cfg)
		if err != nil {
			t.Fatalf("opening: %v", err)
		}
		defer p.Close()

		ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
		defer cancel()
		start := time.Now()
		r := ormhealth.Quick(ctx, p)
		elapsed := time.Since(start)

		if r.OK() {
			t.Fatal("an unreachable database reported up")
		}
		if r.Status != ormhealth.StatusDown {
			t.Errorf("status = %s", r.Status)
		}
		if r.Err == nil {
			t.Error("no error was reported")
		}
		if elapsed > 5*time.Second {
			t.Errorf("a failing readiness check took %v", elapsed)
		}
	})

	t.Run("no database is down rather than a panic", func(t *testing.T) {
		if r := ormhealth.Quick(t.Context(), nil); r.OK() {
			t.Error("a nil database reported up")
		}
	})
}

// Scenario F: the deep check reports connectivity, version, pool, extensions,
// schema and migration state — and mutates nothing.
func TestDeep(t *testing.T) {
	p := pool(t, schema(t))

	t.Run("connectivity, version and pool always", func(t *testing.T) {
		r := ormhealth.Deep(t.Context(), p)
		if !r.OK() {
			t.Fatalf("status = %s\n%s", r.Status, r)
		}
		if r.Version == "" || r.VersionNum == 0 {
			t.Errorf("version = %q (%d)", r.Version, r.VersionNum)
		}
		if r.Pool == nil {
			t.Fatal("no pool stats")
		}
		if r.Pool.Max != 4 {
			t.Errorf("max conns = %d, want the pool's own 4", r.Pool.Max)
		}
		if r.Pool.Saturated {
			t.Error("an idle pool reported saturated")
		}
		// Nothing was asked for, so nothing expensive ran.
		if r.Schema != nil || r.Migrations != nil {
			t.Error("the deep check ran a schema or migration check nobody asked for")
		}
		if len(r.Extensions) != 0 {
			t.Error("the deep check checked extensions nobody named")
		}
	})

	t.Run("an extension that is present and one that is not", func(t *testing.T) {
		r := ormhealth.Deep(t.Context(), p,
			ormhealth.WithExtensions("citext", "postgis"))
		if len(r.Extensions) != 2 {
			t.Fatalf("extensions = %+v", r.Extensions)
		}
		byName := map[string]ormhealth.Extension{}
		for _, e := range r.Extensions {
			byName[e.Name] = e
		}
		// The fixture schema creates citext.
		if e := byName["citext"]; !e.Installed || e.Version == "" {
			t.Errorf("citext = %+v, want installed with a version", e)
		}
		// Scenario: PostGIS absent is reported, not installed.
		if e := byName["postgis"]; e.Installed {
			t.Errorf("postgis reported installed in a database without it")
		}
		if r.Status != ormhealth.StatusDegraded {
			t.Errorf("status = %s, want degraded when a required extension is absent", r.Status)
		}

		// And it was not installed by the checking.
		after := ormhealth.Deep(t.Context(), p, ormhealth.WithExtensions("postgis"))
		if after.Extensions[0].Installed {
			t.Error("the health check installed PostGIS")
		}
	})

	t.Run("schema clean and schema drifted", func(t *testing.T) {
		cfgPath := filepath.Join(repoRoot(t), "internal", "gendemo", "orm.yaml")

		clean := pool(t, schema(t))
		t.Setenv("ORM_GENDEMO_DSN", clean.Config().ConnString())
		r := ormhealth.Deep(t.Context(), clean, ormhealth.WithSchemaCheck(cfgPath))
		if r.Schema == nil {
			t.Fatal("no schema state")
		}
		if r.Schema.Status != ormhealth.StatusUp {
			t.Errorf("a matching schema reported %s (%d findings, %v)",
				r.Schema.Status, r.Schema.Findings, r.Schema.Codes)
		}
	})

	t.Run("drift is reported with codes and nothing is changed", func(t *testing.T) {
		cfgPath := filepath.Join(repoRoot(t), "internal", "gendemo", "orm.yaml")
		drifted := pool(t, schema(t)+"\nALTER TABLE users DROP COLUMN nickname;")
		t.Setenv("ORM_GENDEMO_DSN", drifted.Config().ConnString())

		r := ormhealth.Deep(t.Context(), drifted, ormhealth.WithSchemaCheck(cfgPath))
		if r.Schema == nil || r.Schema.Status != ormhealth.StatusDegraded {
			t.Fatalf("schema = %+v, want degraded", r.Schema)
		}
		if r.Schema.Findings == 0 || len(r.Schema.Codes) == 0 {
			t.Errorf("no findings or codes: %+v", r.Schema)
		}
		if r.Status != ormhealth.StatusDegraded {
			t.Errorf("overall status = %s", r.Status)
		}

		// Release-critical: the check read the database and left it drifted.
		var exists bool
		if err := drifted.QueryRow(t.Context(),
			`SELECT EXISTS (SELECT 1 FROM information_schema.columns
			 WHERE table_name = 'users' AND column_name = 'nickname')`).Scan(&exists); err != nil {
			t.Fatalf("checking the column: %v", err)
		}
		if exists {
			t.Error("the health check repaired the schema it was asked to inspect")
		}
	})

	t.Run("a database that did not answer stops there", func(t *testing.T) {
		cfg, _ := pgxpool.ParseConfig("postgres://nobody@127.0.0.1:1/none?sslmode=disable&connect_timeout=1")
		p, err := pgxpool.NewWithConfig(t.Context(), cfg)
		if err != nil {
			t.Fatalf("opening: %v", err)
		}
		defer p.Close()
		ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
		defer cancel()

		r := ormhealth.Deep(ctx, p, ormhealth.WithExtensions("postgis"))
		if r.Status != ormhealth.StatusDown {
			t.Errorf("status = %s", r.Status)
		}
		// Asking anything else of a dead database would turn one failure into
		// several timeouts.
		if len(r.Extensions) != 0 || r.Version != "" {
			t.Errorf("the deep check kept going after connectivity failed: %+v", r)
		}
	})
}

// Release-critical: nothing in a report is a secret. A health endpoint is often
// the least authenticated thing a service exposes.
func TestDeep_reportsNoSecrets(t *testing.T) {
	const password = "sentinel-health-pa55word"
	testdb.AdminDSN(t)
	dsn := testdb.Create(t, schema(t))

	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatalf("parsing: %v", err)
	}
	// The password in the configuration is the thing that must not appear.
	realPassword := cfg.ConnConfig.Password
	p, err := pgxpool.NewWithConfig(t.Context(), cfg)
	if err != nil {
		t.Fatalf("opening: %v", err)
	}
	t.Cleanup(p.Close)

	r := ormhealth.Deep(t.Context(), p, ormhealth.WithExtensions("citext"))
	rendered := r.String()

	for _, secret := range []string{realPassword, password, dsn} {
		if secret == "" {
			continue
		}
		if strings.Contains(rendered, secret) {
			t.Errorf("the report carries %q:\n%s", secret, rendered)
		}
	}
	// It does carry the useful facts.
	for _, want := range []string{"status", "connectivity", "postgresql", "pool", "extension citext"} {
		if !strings.Contains(rendered, want) {
			t.Errorf("the report is missing %q:\n%s", want, rendered)
		}
	}
	// And it says what it did not do.
	if !strings.Contains(rendered, "only reads") {
		t.Errorf("the report does not state that it changed nothing:\n%s", rendered)
	}
}

// Release-critical: the migration state must agree with the migration engine.
//
// This is the check that did not exist, and its absence hid a defect: the
// history query named a column the engine does not write, so it always failed,
// and the failure path reported "nothing has been migrated" about a database
// that was fully up to date. Every part of that answer was wrong and none of it
// carried an error. The test applies real migrations with the real engine and
// then asks what the health check sees.
func TestDeep_migrationStateMatchesTheEngine(t *testing.T) {
	dir := t.TempDir()
	migration := func(id, up, down string) string {
		return `{
		  "format": 1,
		  "id": "` + id + `",
		  "atomic": true,
		  "operations": [
		    {"op": "raw_sql", "args": {"Up": "` + up + `", "Down": "` + down + `", "Atomic": true}}
		  ]
		}`
	}
	first := migration("0001_first",
		"CREATE TABLE health_migration_probe (id bigint PRIMARY KEY)",
		"DROP TABLE health_migration_probe")
	second := migration("0002_second",
		"ALTER TABLE health_migration_probe ADD COLUMN note text",
		"ALTER TABLE health_migration_probe DROP COLUMN note")
	write := func(name, body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("0001_first.json", first)

	// The fixture schema, because the pool helper registers its types on every
	// connection. The probe table this test migrates is its own.
	p := pool(t, schema(t))

	// Before anything is applied: everything is pending, and that is a fact
	// rather than an error.
	r := ormhealth.Deep(t.Context(), p, ormhealth.WithMigrationState(dir))
	if r.Migrations == nil {
		t.Fatal("no migration state")
	}
	if r.Migrations.Applied != 0 || r.Migrations.Pending != 1 {
		t.Errorf("an unmigrated database reported %d applied, %d pending",
			r.Migrations.Applied, r.Migrations.Pending)
	}
	if r.Migrations.Err != nil {
		t.Errorf("an unmigrated database is not an error: %v", r.Migrations.Err)
	}

	// Apply it with the engine the CLI uses.
	conn, err := pgx.Connect(t.Context(), p.Config().ConnString())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(context.Background())
	ormtest.Migrate(t, conn, dir)

	r = ormhealth.Deep(t.Context(), p, ormhealth.WithMigrationState(dir))
	if r.Migrations.Err != nil {
		t.Fatalf("reading the history of a migrated database: %v", r.Migrations.Err)
	}
	if r.Migrations.Applied != 1 || r.Migrations.Pending != 0 {
		t.Errorf("a fully migrated database reported %d applied, %d pending (%v)",
			r.Migrations.Applied, r.Migrations.Pending, r.Migrations.PendingIDs)
	}
	if r.Migrations.Status != ormhealth.StatusUp {
		t.Errorf("migration status = %s, want up", r.Migrations.Status)
	}
	if !r.OK() {
		t.Errorf("overall status = %s for a migrated database\n%s", r.Status, r)
	}

	// A migration the project has and the database does not is the state a
	// deploy check exists to catch.
	write("0002_second.json", second)
	r = ormhealth.Deep(t.Context(), p, ormhealth.WithMigrationState(dir))
	if r.Migrations.Applied != 1 || r.Migrations.Pending != 1 {
		t.Fatalf("state = %+v, want one applied and one pending", r.Migrations)
	}
	if len(r.Migrations.PendingIDs) != 1 || r.Migrations.PendingIDs[0] != "0002_second" {
		t.Errorf("pending = %v, want the second migration named", r.Migrations.PendingIDs)
	}
	if r.Migrations.Status != ormhealth.StatusDegraded || r.Status != ormhealth.StatusDegraded {
		t.Errorf("a database behind the project reported %s / %s", r.Migrations.Status, r.Status)
	}

	// And the check did not apply it.
	var present bool
	if err := p.QueryRow(t.Context(),
		`SELECT EXISTS (SELECT 1 FROM information_schema.columns
		 WHERE table_name = 'health_migration_probe' AND column_name = 'note')`).Scan(&present); err != nil {
		t.Fatal(err)
	}
	if present {
		t.Error("the health check applied the migration it was asked to report on")
	}
}

// A history table that cannot be read is unknown, not empty.
//
// The two are opposite answers — "your database has never been migrated" is an
// emergency, "I could not tell" is a page for whoever owns the check — and the
// error path used to give the first one for both.
func TestDeep_unreadableHistoryIsUnknown(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "0001_first.json"), []byte(`{
	  "format": 1,
	  "id": "0001_first",
	  "atomic": true,
	  "operations": [
	    {"op": "raw_sql", "args": {"Up": "SELECT 1", "Down": "SELECT 1", "Atomic": true}}
	  ]
	}`), 0o600); err != nil {
		t.Fatal(err)
	}

	p := pool(t, schema(t))

	// A history table of the wrong shape stands in for every way reading it can
	// fail that is not the table being absent.
	if _, err := p.Exec(t.Context(),
		`CREATE TABLE orm_schema_migrations (wrong_column text)`); err != nil {
		t.Fatal(err)
	}

	r := ormhealth.Deep(t.Context(), p, ormhealth.WithMigrationState(dir))
	if r.Migrations == nil {
		t.Fatal("no migration state")
	}
	if r.Migrations.Status != ormhealth.StatusUnknown {
		t.Errorf("status = %s, want unknown when the history cannot be read", r.Migrations.Status)
	}
	if r.Migrations.Err == nil {
		t.Error("a failed read reported no error")
	}
	if r.Migrations.Pending != 0 {
		t.Errorf("a failed read invented %d pending migrations", r.Migrations.Pending)
	}
}
