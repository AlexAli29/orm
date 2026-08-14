// Package gen wires the generator's stages together.
//
// Each stage is independently usable and independently tested; this package
// exists so the CLI does not have to know the order they go in.
package gen

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/AlexAli29/orm/gen/diag"
	"github.com/AlexAli29/orm/internal/gen/config"
	"github.com/AlexAli29/orm/internal/gen/emit"
	"github.com/AlexAli29/orm/internal/gen/goscan"
	"github.com/AlexAli29/orm/internal/gen/model"
	"github.com/AlexAli29/orm/internal/gen/pgintro"
	"github.com/AlexAli29/orm/internal/gen/reconcile"
)

// Result is one complete run of the pipeline.
type Result struct {
	Mapping *model.Mapping
	Report  *diag.Report
	// Schema is the introspected schema the report was produced against.
	Schema *model.Schema
	// Entities are the entities the scanner discovered, including the ones that
	// failed reconciliation.
	Entities []*model.GoEntity
	// Packages describes the scanned packages, including the identifiers they
	// already declare.
	Packages []goscan.Package
}

// Check loads the entities, introspects the schema and reconciles them.
//
// An error here is a failure of the tool — an unreachable database, a package
// that does not compile — and is distinct from a report full of findings, which
// is the tool working. Callers map the two onto different exit codes.
func Check(ctx context.Context, cfg *config.Config) (*Result, error) {
	schema, err := loadSchema(ctx, cfg)
	if err != nil {
		return nil, err
	}

	targets := make([]goscan.Target, 0, len(cfg.Packages))
	for _, p := range cfg.Packages {
		targets = append(targets, goscan.Target{Dir: cfg.Dir(p), OutputDir: cfg.OutputDir(p)})
	}
	scanned, err := goscan.Scan(ctx, cfg.Root, targets)
	if err != nil {
		return nil, err
	}
	if len(scanned.Entities) == 0 {
		// A check that examined nothing has proved nothing, and reporting it
		// as clean would be a lie — the most dangerous one this tool could
		// tell, because the whole point is to be believed. A packages.path
		// pointing at the wrong directory is the usual cause, and it produces
		// exactly this: a green check over an empty set.
		paths := make([]string, 0, len(cfg.Packages))
		for _, p := range cfg.Packages {
			paths = append(paths, p.Path)
		}
		return nil, fmt.Errorf("no entities found in %s: mark your entity structs with //orm:table, or correct packages.path",
			strings.Join(paths, ", "))
	}

	mapping, report := reconcile.Run(reconcile.Input{
		Config:    cfg,
		Entities:  scanned.Entities,
		Schema:    schema,
		TagErrors: scanned.TagErrors,
	})
	// The view half, into the same report. A check that reconciled kinds and
	// columns but left bodies, dependencies, options and indexes unexamined
	// would report clean over four kinds of drift.
	if err := checkViews(ctx, cfg, report, scanned.Entities, schema); err != nil {
		return nil, err
	}
	return &Result{
		Mapping:  mapping,
		Report:   report,
		Schema:   schema,
		Entities: scanned.Entities,
		Packages: scanned.Packages,
	}, nil
}

// Generate reconciles and then renders the generated code, without writing it.
//
// Generation is refused unless the report clears the threshold. Code emitted
// from a mapping that does not hold would be code that compiles against a
// schema it disagrees with, which is the failure this project exists to
// prevent; producing it because the author asked twice would be worse than
// refusing.
func Generate(ctx context.Context, cfg *config.Config, threshold diag.Severity) (*Result, []emit.File, error) {
	result, err := Check(ctx, cfg)
	if err != nil {
		return nil, nil, err
	}
	if result.Report.Failed(threshold) {
		return result, nil, ErrUnproven
	}

	reserved := make(map[string][]string, len(result.Packages))
	for _, pkg := range result.Packages {
		names := make([]string, 0, len(pkg.Idents))
		for name, file := range pkg.Idents {
			// A previous run's own output is not a collision; it is what this
			// run replaces. Counting it would let generation succeed once and
			// fail ever after.
			if !emit.IsGenerated(file) {
				names = append(names, name)
			}
		}
		slices.Sort(names)
		reserved[pkg.Path] = names
	}

	files, err := emit.Generate(emit.Input{Mapping: result.Mapping, Reserved: reserved})
	if err != nil {
		return result, nil, err
	}
	return result, files, nil
}

// ErrUnproven reports that generation was refused because reconciliation did
// not hold. The report is on the Result the caller already has.
var ErrUnproven = errors.New("the mapping is not proven")

// Write writes rendered files, replacing whatever is there.
//
// Each file lands through a temporary file in the same directory and a rename,
// so a reader either sees the previous content or the new content and never a
// half-written file. Callers render everything before calling this, so a
// failure to generate cannot leave a package half-updated.
func Write(files []emit.File) error {
	for _, f := range files {
		if err := writeAtomic(f.Path, f.Content); err != nil {
			return err
		}
	}
	return nil
}

func writeAtomic(path string, content []byte) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".*")
	if err != nil {
		return fmt.Errorf("creating a temporary file in %s: %w", dir, err)
	}
	name := tmp.Name()
	defer func() { _ = os.Remove(name) }()

	if _, err := tmp.Write(content); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("writing %s: %w", name, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("closing %s: %w", name, err)
	}
	// Generated files are read by people and by the Go tool, not written to,
	// so they get the same permissions as ordinary source.
	if err := os.Chmod(name, 0o644); err != nil {
		return fmt.Errorf("setting the mode of %s: %w", name, err)
	}
	if err := os.Rename(name, path); err != nil {
		return fmt.Errorf("replacing %s: %w", path, err)
	}
	return nil
}

// loadSchema reads the schema from wherever the configuration says it lives.
func loadSchema(ctx context.Context, cfg *config.Config) (*model.Schema, error) {
	if cfg.Schema.FromFile() {
		schema, err := pgintro.FromFile(ctx, cfg.Schema.AdminDSN, cfg.SchemaFile(), cfg.Schema.SearchPath)
		if err != nil {
			return nil, fmt.Errorf("building the schema from %s: %w", cfg.Schema.File, err)
		}
		return schema, nil
	}

	conn, err := pgintro.Connect(ctx, cfg.Schema.DSN)
	if err != nil {
		return nil, err
	}
	defer func() { _ = conn.Close(context.WithoutCancel(ctx)) }()

	schema, err := pgintro.Introspect(ctx, conn, cfg.Schema.SearchPath)
	if err != nil {
		return nil, fmt.Errorf("introspecting the schema: %w", err)
	}
	return schema, nil
}
