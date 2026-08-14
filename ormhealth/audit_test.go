package ormhealth_test

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/AlexAli29/orm"
	"github.com/AlexAli29/orm/internal/testdb"
	"github.com/AlexAli29/orm/observe"
	"github.com/AlexAli29/orm/ormhealth"
	"github.com/jackc/pgx/v5/pgxpool"
)

// The M15 final audit: attacks on ormhealth.
//
// These go after the two things a health package is most likely to get quietly
// wrong — reporting on a database other than the one it was handed, and losing
// a fact because the caller wrapped their pool.

// Release-critical: Deep is given a Querier, and everything it reports must be
// about *that* database.
//
// The schema check runs the project's reconciliation from a configuration file,
// and that file names its own DSN. If the check follows the file rather than the
// executor, an operator pointed at one database can be told about another — and
// the report will look completely normal while doing it.
func TestAudit_deepSchemaCheckFollowsTheExecutor(t *testing.T) {
	testdb.AdminDSN(t)
	cfgPath := filepath.Join(repoRoot(t), "internal", "gendemo", "orm.yaml")

	// Two databases: one matching the Go types, one drifted.
	clean := pool(t, schema(t))
	drifted := pool(t, schema(t)+"\nALTER TABLE users DROP COLUMN nickname;")

	// The configuration points at the CLEAN one.
	t.Setenv("ORM_GENDEMO_DSN", clean.Config().ConnString())

	// The health check is handed the DRIFTED one.
	r := ormhealth.Deep(t.Context(), drifted, ormhealth.WithSchemaCheck(cfgPath))
	if r.Schema == nil {
		t.Fatal("no schema state")
	}
	if r.Schema.Status == ormhealth.StatusUp {
		t.Errorf("Deep was handed a drifted database and reported its schema clean —\n"+
			"it reconciled whatever the configuration file names, not the executor it was given.\n"+
			"An operator checking one database would be told about another.\nreport:\n%s", r)
	}
}

// A wrapped executor is the normal production shape — a tracer around the pool —
// and it must not make facts disappear.
func TestAudit_poolStatsSurviveATracedExecutor(t *testing.T) {
	p := pool(t, schema(t))

	plain := ormhealth.Deep(t.Context(), p)
	if plain.Pool == nil {
		t.Fatal("a bare pool reported no pool stats")
	}

	traced := orm.Traced(p, noopTracer{})
	wrapped := ormhealth.Deep(t.Context(), traced)
	if wrapped.Pool == nil {
		t.Error("wrapping the pool in a tracer lost the pool statistics; " +
			"a traced executor is the ordinary production shape")
	}
}

type noopTracer struct{}

func (noopTracer) Start(ctx context.Context, e observe.StartEvent) context.Context { return ctx }
func (noopTracer) End(ctx context.Context, e observe.EndEvent)                     {}

// A typed nil executor must be reported as down rather than panicking.
func TestAudit_typedNilExecutor(t *testing.T) {
	var p *pgxpool.Pool // typed nil: the interface is not nil
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("a typed-nil executor panicked: %v", r)
		}
	}()
	if r := ormhealth.Quick(t.Context(), p); r.OK() {
		t.Error("a nil pool reported up")
	}
}

// The report must never carry tuning language: pool saturation is a fact.
func TestAudit_noTuningLanguage(t *testing.T) {
	p := pool(t, schema(t))
	r := ormhealth.Deep(t.Context(), p, ormhealth.WithExtensions("citext"))
	text := strings.ToLower(r.String())
	for _, phrase := range []string{
		"increase", "decrease", "should be", "recommend", "set maxconns",
		"tune", "consider raising", "too small", "too large",
	} {
		if strings.Contains(text, phrase) {
			t.Errorf("the report contains tuning language (%q):\n%s", phrase, r)
		}
	}
}
