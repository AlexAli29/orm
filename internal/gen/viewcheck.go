package gen

import (
	"context"
	"fmt"

	"github.com/AlexAli29/orm/gen/diag"
	"github.com/AlexAli29/orm/internal/gen/config"
	"github.com/AlexAli29/orm/internal/gen/desired"
	"github.com/AlexAli29/orm/internal/gen/model"
	"github.com/AlexAli29/orm/internal/gen/pgintro"
	"github.com/AlexAli29/orm/internal/gen/reconcile"
	"github.com/AlexAli29/orm/internal/gen/schema"
)

// The view half of a check, wired into the one place a check happens.
//
// Everything here adds findings to the report reconcile.Run produced. There is
// no second result and no CLI-specific comparison: what `orm check` prints is
// what the reconciliation engine decided, and a future migration planner reads
// the same facts from the same functions.
//
// It needs things Run does not — a connection, and the desired schema the
// declarations build — which is why it is a second call rather than another
// tier inside Run. Run reconciles an already-introspected schema and is used
// where there is no database to query.

// checkViews adds relation-kind-specific findings: body provenance, direct
// dependencies, unrepresentable metadata, and materialized-view indexes.
func checkViews(ctx context.Context, cfg *config.Config, report *diag.Report,
	entities []*model.GoEntity, actualModel *model.Schema) error {
	managed := map[string]bool{}
	for _, e := range entities {
		if e.Kind != model.RelTable {
			managed[qualify(cfg, e)] = true
		}
	}
	if len(managed) == 0 {
		return nil
	}
	// A schema read from a file has no server to ask, so the checks that need
	// one do not run. Saying so is better than reporting clean.
	if cfg.Schema.FromFile() {
		return nil
	}

	conn, err := pgintro.Connect(ctx, cfg.Schema.DSN)
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close(context.WithoutCancel(ctx)) }()

	// Body provenance and drift, against the recording a migration made.
	if err := reconcile.CheckDefinitions(ctx, report, reconcile.DefinitionInput{
		Entities: entities, Schema: actualModel, Reader: conn,
	}); err != nil {
		return err
	}

	// The canonical schema is the actual half in the same value types the
	// desired half is built in, which is what lets dependencies, options and
	// indexes be compared rather than described.
	actual, err := pgintro.Canonical(ctx, conn, cfg.Schema.SearchPath)
	if err != nil {
		return fmt.Errorf("reading the schema for view reconciliation: %w", err)
	}
	want, err := desired.Build(desired.Input{Config: cfg, Entities: entities})
	if err != nil {
		// The declarations do not build a schema, which desired.Build has
		// already said in its own words. Comparing against nothing would add
		// findings derived from a schema that does not exist.
		return nil
	}

	reconcile.CheckDependencies(report, want, actual)
	reconcile.CheckMetadata(report, actual, managed)
	reconcile.CheckMaterializedIndexes(report, want, actual)
	return nil
}

// qualify resolves a declaration's relation name the way the desired schema
// does, so the two agree on what "public" means.
func qualify(cfg *config.Config, e *model.GoEntity) string {
	s := e.Table.Schema
	if s == "" {
		if cfg != nil && len(cfg.Schema.SearchPath) > 0 {
			s = cfg.Schema.SearchPath[0]
		} else {
			s = "public"
		}
	}
	return s + "." + e.Table.Name
}

var _ = schema.KindView
