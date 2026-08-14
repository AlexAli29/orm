// Package ormhealth reports what is true about a database, and changes nothing.
//
// There are two checks and the difference between them is the point.
//
// [Quick] asks whether the database answers. It is one round trip and is what a
// readiness probe should call — a probe that runs every few seconds must cost
// almost nothing, and a probe that reconciles a schema on every call is an
// outage waiting for a deployment.
//
// [Deep] asks the questions an operator asks when something is wrong: which
// version, how the pool is doing, whether the schema still matches the Go types,
// whether the extensions the project declared are installed, whether migrations
// are outstanding. It costs several round trips and a catalog read, and it
// belongs behind an operational endpoint or a command rather than a probe.
//
// # Everything here is read-only
//
// No function in this package migrates, creates an extension, creates an index,
// runs ANALYZE or VACUUM, alters a setting, or resizes a pool. A health check
// that repaired what it found would be a health check nobody could safely call
// from a probe, and "the readiness endpoint migrated production" is a sentence
// that should never be true. The mutation APIs exist elsewhere and are invoked
// deliberately; a repository-wide test asserts this package never reaches them.
//
// # It reports facts, not advice
//
// A saturated pool is reported as saturated, with the numbers pgx gave. What to
// do about it depends on the service, the workload and the database's own
// limits, none of which this package can see — so it does not suggest a number.
// That is the same line M14's diagnostics draw.
//
// # It says nothing secret
//
// A report carries no DSN, no password, no bind value and no statistics content.
// The version string, the pool's counters, a schema finding's diagnostic code —
// all structure. A health endpoint is often the least authenticated thing a
// service exposes, so it is the last place a credential should be able to reach.
package ormhealth

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/AlexAli29/orm"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Status is the outcome of a check.
type Status string

// The statuses.
const (
	// StatusUp means the check answered and found nothing wrong.
	StatusUp Status = "up"
	// StatusDegraded means the database is reachable but something a check
	// looked at is not as the project declared it. A service can usually still
	// serve traffic.
	StatusDegraded Status = "degraded"
	// StatusDown means the database did not answer.
	StatusDown Status = "down"
	// StatusUnknown means a check could not run. It is not a failure of the
	// thing being checked: a schema check needs a configuration file, and its
	// absence says nothing about the database.
	StatusUnknown Status = "unknown"
)

// Querier is what these checks need: the ability to run a statement.
//
// It is the ORM's Executor by shape, so a pool, a connection or a transaction
// all satisfy it.
type Querier interface {
	Query(ctx context.Context, sql string, args ...any) (pgxRows, error)
}

// QuickReport is what a readiness probe gets.
type QuickReport struct {
	// Status is the worst state anything in this report reached.
	Status Status
	// Latency is how long the round trip took.
	Latency time.Duration
	// Err is why the database did not answer, or nil.
	Err error
}

// OK reports whether the database answered.
func (r QuickReport) OK() bool { return r.Status == StatusUp }

// Quick asks the database whether it is there.
//
// It is one statement — SELECT 1 — and nothing else: no catalog read, no schema
// reconciliation, no version lookup. That is what makes it safe to call every
// two seconds from a probe, and it is the whole design.
//
// It answers within the context's deadline. A probe should give it one; without
// one a database that has stopped answering will block for as long as the
// connection attempt takes.
func Quick(ctx context.Context, q Querier) QuickReport {
	start := time.Now()
	if !alive(q) {
		return QuickReport{Status: StatusDown, Err: fmt.Errorf("ormhealth: no database")}
	}
	rows, err := q.Query(ctx, "SELECT 1")
	if err != nil {
		return QuickReport{Status: StatusDown, Latency: time.Since(start), Err: err}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return QuickReport{Status: StatusDown, Latency: time.Since(start), Err: err}
	}
	return QuickReport{Status: StatusUp, Latency: time.Since(start)}
}

// PoolStats is what pgx knows about a pool.
//
// The numbers are pgx's own, read at the moment of the call. Nothing here keeps
// a counter of its own: pgx is the source of truth for its pool, and a second
// tally would be a second answer that could disagree.
type PoolStats struct {
	// Acquired is how many connections are checked out right now; Idle is how
	// many are open and free; Total is the two together. Max is the ceiling the
	// pool was configured with, and Constructing counts connections currently
	// being opened.
	Acquired     int32
	Idle         int32
	Total        int32
	Max          int32
	Constructing int32
	// AcquireCount is how many times a connection has been taken from the pool
	// since it opened. EmptyAcquireCount is how many of those had to wait
	// because none was free, and CanceledAcquires how many gave up first —
	// together they say whether the pool is the thing a request waits on.
	AcquireCount      int64
	EmptyAcquireCount int64
	CanceledAcquires  int64
	// Saturated reports that every connection the pool may open is checked out.
	//
	// It is a fact, not a recommendation. What to do about it depends on the
	// service, the workload and the server's own connection limit, and this
	// package can see none of the three.
	Saturated bool
}

// Extension is whether a PostgreSQL extension is installed.
type Extension struct {
	// Name is the extension as PostgreSQL names it, and Installed reports
	// whether it is present in this database — not merely available to install.
	Name      string
	Installed bool
	// Version is what the server reports, empty when the extension is absent.
	Version string
}

// SchemaState is what a schema check found.
type SchemaState struct {
	// Status is the worst state anything in this report reached.
	Status Status
	// Findings is how many errors the reconciliation reported.
	Findings int
	// Codes lists the diagnostic codes, deduplicated and sorted, so that a
	// report can be compared between runs and alerted on. The findings'
	// messages are not here: they name columns and types, which is fine, but
	// the codes are the stable part.
	Codes []string
	// Err is why the check could not run.
	Err error
}

// MigrationState is what the migration engine knows.
type MigrationState struct {
	// Status is the worst state anything in this report reached.
	Status Status
	// Applied is how many migrations the database records.
	Applied int
	// Pending is how many the project has that the database does not.
	Pending int
	// PendingIDs names them, in order.
	PendingIDs []string
	// Err is why the state could not be read.
	Err error
}

// DeepReport is what an operator gets.
type DeepReport struct {
	// Status is the worst state anything in this report reached.
	Status Status
	// Connectivity is the same check [Quick] runs.
	Connectivity QuickReport
	// Version is what the server calls itself, and VersionNum is the numeric
	// form — 170002 for 17.2 — which is what a comparison should use.
	Version    string
	VersionNum int
	// Pool is present when the check was given a pool rather than a bare
	// connection.
	Pool *PoolStats
	// Extensions is one entry per extension the caller asked about.
	Extensions []Extension
	// Schema and Migrations are present when the caller asked for them.
	Schema     *SchemaState
	Migrations *MigrationState
}

// OK reports whether every check that ran found nothing wrong.
func (r DeepReport) OK() bool { return r.Status == StatusUp }

// String renders the report for a person or a log.
//
// It carries no DSN, no credential and no bind value, because none of those is
// in the report to begin with.
func (r DeepReport) String() string {
	var b strings.Builder
	fmt.Fprintf(&b, "status: %s\n", r.Status)
	fmt.Fprintf(&b, "connectivity: %s (%s)\n", r.Connectivity.Status, r.Connectivity.Latency.Round(time.Microsecond))
	if r.Connectivity.Err != nil {
		fmt.Fprintf(&b, "  error: %v\n", r.Connectivity.Err)
	}
	if r.Version != "" {
		fmt.Fprintf(&b, "postgresql: %s (%d)\n", r.Version, r.VersionNum)
	}
	if p := r.Pool; p != nil {
		fmt.Fprintf(&b, "pool: %d acquired, %d idle, %d of max %d",
			p.Acquired, p.Idle, p.Total, p.Max)
		if p.Saturated {
			b.WriteString(" — saturated")
		}
		b.WriteString("\n")
	}
	for _, e := range r.Extensions {
		if e.Installed {
			fmt.Fprintf(&b, "extension %s: installed (%s)\n", e.Name, e.Version)
		} else {
			fmt.Fprintf(&b, "extension %s: absent\n", e.Name)
		}
	}
	if s := r.Schema; s != nil {
		fmt.Fprintf(&b, "schema: %s", s.Status)
		if s.Findings > 0 {
			fmt.Fprintf(&b, " (%d findings: %s)", s.Findings, strings.Join(s.Codes, ", "))
		}
		if s.Err != nil {
			fmt.Fprintf(&b, " — %v", s.Err)
		}
		b.WriteString("\n")
	}
	if m := r.Migrations; m != nil {
		fmt.Fprintf(&b, "migrations: %s, %d applied, %d pending", m.Status, m.Applied, m.Pending)
		if len(m.PendingIDs) > 0 {
			fmt.Fprintf(&b, " (%s)", strings.Join(m.PendingIDs, ", "))
		}
		if m.Err != nil {
			fmt.Fprintf(&b, " — %v", m.Err)
		}
		b.WriteString("\n")
	}
	b.WriteString("nothing above was changed: this package only reads.\n")
	return b.String()
}

// poolOf returns the pgx pool behind a querier, when there is one.
//
// It unwraps tracing first. Attaching a tracer is the ordinary production shape,
// and a check that lost the pool's statistics the moment somebody instrumented
// their database would report less exactly where it is needed most.
func poolOf(q Querier) *pgxpool.Pool {
	if ex, ok := q.(orm.Executor); ok {
		if p, ok := orm.Unwrap(ex).(*pgxpool.Pool); ok {
			return p
		}
	}
	p, _ := q.(*pgxpool.Pool)
	return p
}

// alive reports whether a querier is usable at all.
//
// A typed nil — a (*pgxpool.Pool)(nil) held in the interface — is not the same
// as a nil interface, and calling Query on one panics. A health check is the
// last thing that should crash a probe, so both are treated as "no database".
func alive(q Querier) bool {
	switch v := q.(type) {
	case nil:
		return false
	case *pgxpool.Pool:
		return v != nil
	default:
		return true
	}
}

// statsOf reads pgx's own counters.
func statsOf(p *pgxpool.Pool) *PoolStats {
	if p == nil {
		return nil
	}
	s := p.Stat()
	return &PoolStats{
		Acquired:          s.AcquiredConns(),
		Idle:              s.IdleConns(),
		Total:             s.TotalConns(),
		Max:               s.MaxConns(),
		Constructing:      s.ConstructingConns(),
		AcquireCount:      s.AcquireCount(),
		EmptyAcquireCount: s.EmptyAcquireCount(),
		CanceledAcquires:  s.CanceledAcquireCount(),
		Saturated:         s.MaxConns() > 0 && s.AcquiredConns() >= s.MaxConns(),
	}
}
