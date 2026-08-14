package main

import (
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/AlexAli29/orm/internal/gen/lock"
	"github.com/AlexAli29/orm/internal/gen/managed"
)

// The four states a managed project has, reported apart.
//
// "The schema does not match" is four different problems wearing one sentence,
// and each has a different fix: write a migration, run a migration, repair the
// database, regenerate the code. Collapsing them would leave the reader to
// guess which — and the guess that costs the most is treating an unrun
// migration as drift, because the repair for drift is to change the database by
// hand, which is what caused the drift the tool actually means.

// stateReport is what a managed check found, one line per dimension.
type stateReport struct {
	// Lines are the summary lines, in a fixed order.
	Lines []string
	// Detail elaborates on whatever was not clean, in the same order.
	Detail []string
	// Failed reports whether anything reached the threshold for a non-zero exit.
	Failed bool
}

// managedState gathers and describes the model, migration and database states.
//
// The mapping and generated dimensions are supplied by the caller, which has
// already computed them: reconciliation is the same work in every mode, and
// doing it twice would be two connections and two scans to answer one question.
func managedState(ctx context.Context, p *managed.Project, mapping string, mappingFailed bool, generated lock.State, askedAboutGenerated bool) (stateReport, error) {
	var r stateReport
	add := func(name, value string) { r.Lines = append(r.Lines, fmt.Sprintf("%-12s %s", name, value)) }
	detail := func(format string, args ...any) { r.Detail = append(r.Detail, fmt.Sprintf(format, args...)) }

	conn, closeConn, err := connect(ctx, p)
	if err != nil {
		return r, err
	}
	defer closeConn()

	snapshot, err := p.Snapshot(ctx, managed.SnapshotInput{Conn: conn})
	if err != nil {
		return r, err
	}
	c := snapshot.Comparison

	switch {
	case c.NeedsMigration():
		add("Models", fmt.Sprintf("%d change(s) no migration describes", len(c.ModelChanges)))
		r.Failed = true
		detail("Models have schema changes not represented by migrations.\n")
		for _, d := range c.ModelChanges {
			detail("    %s", d)
		}
		detail("\nRun:\n    orm makemigrations\n")
	case snapshot.Set.Len() == 0:
		add("Models", "no migrations describe them yet")
		r.Failed = true
		detail("There are no migrations, and the models describe no schema either.\n")
	default:
		add("Models", "match the latest migration")
	}

	switch {
	case c.NeedsMigrate():
		add("Migrations", fmt.Sprintf("%d applied, %d pending", len(snapshot.Applied), len(c.PendingMigrations)))
		r.Failed = true
		detail("Pending migrations:\n")
		for _, id := range c.PendingMigrations {
			detail("    %s", id)
		}
		detail("\nRun:\n    orm migrate\n")
	case snapshot.Set.Len() == 0:
		add("Migrations", "none")
	default:
		add("Migrations", fmt.Sprintf("%d applied, none pending", len(snapshot.Applied)))
	}

	switch {
	case c.HasDrift():
		add("Database", fmt.Sprintf("drift: %d difference(s) from the applied migrations", len(c.Drift)))
		r.Failed = true
		detail("Database drift detected.\n\n" +
			"The applied migrations describe a schema the database does not have. Something changed it\n" +
			"outside the migrations; nothing here will quietly rewrite the migration state to match.\n")
		for _, d := range c.Drift {
			detail("    %s", d)
		}
		detail("\nEither undo the change, or write a migration that describes it and record it with\n" +
			"    orm migrate --fake <migration>\n")
	case c.NeedsMigrate():
		add("Database", "behind the migrations")
	default:
		add("Database", "fully migrated, no drift")
	}

	add("Mapping", mapping)
	if mappingFailed {
		r.Failed = true
	}

	switch generated {
	case lock.Stale:
		add("Generated", "stale: produced from a different mapping")
		r.Failed = true
		detail("The generated code was produced from a different mapping.\n\nRun:\n    orm generate\n")
	case lock.Unknown:
		add("Generated", "written by a different version of this tool")
		r.Failed = true
		detail("Run:\n    orm generate\n")
	case lock.Missing:
		// A project that has never generated is a project mid-setup, and the
		// workflow this tool recommends runs check before the first generate.
		// Failing there would make it hostile to its own instructions.
		add("Generated", "never written")
		r.Failed = r.Failed || askedAboutGenerated
		detail("This project has not generated code yet.\n\nRun:\n    orm generate\n")
	default:
		add("Generated", "current")
	}
	return r, nil
}

// write prints the summary block and whatever needed elaborating.
func (r stateReport) write(w io.Writer) {
	fmt.Fprintln(w)
	for _, line := range r.Lines {
		fmt.Fprintf(w, "%s\n", line)
	}
	for _, d := range r.Detail {
		if strings.HasPrefix(d, "    ") || strings.HasPrefix(d, "\n") {
			fmt.Fprintf(w, "%s\n", d)
			continue
		}
		fmt.Fprintf(w, "\n%s\n", d)
	}
}

// preflight refuses generation in managed mode unless the three schema states
// agree.
//
// The generator reads the live database, because that is where the mapping it
// proves has to hold. So generating while the database is not the schema the
// models and migrations agree on would bake an unreviewed schema into the
// runtime types — code that compiles, passes its own check, and describes a
// database nobody approved.
func preflight(ctx context.Context, p *managed.Project) error {
	conn, closeConn, err := connect(ctx, p)
	if err != nil {
		return err
	}
	defer closeConn()

	snapshot, err := p.Snapshot(ctx, managed.SnapshotInput{Conn: conn})
	if err != nil {
		return err
	}
	c := snapshot.Comparison
	switch {
	case c.NeedsMigration():
		return fmt.Errorf("schema changes are not represented by migrations; run orm makemigrations\n\n    %s",
			strings.Join(c.ModelChanges, "\n    "))
	case c.NeedsMigrate():
		return fmt.Errorf("the database is behind the migrations; run orm migrate\n\n    pending: %s",
			strings.Join(c.PendingMigrations, ", "))
	case c.HasDrift():
		return fmt.Errorf("the database differs from what its applied migrations describe, so generated code would"+
			" describe a schema no migration explains\n\n    %s", strings.Join(c.Drift, "\n    "))
	}
	return nil
}
