package main

import (
	"context"

	"github.com/AlexAli29/orm/internal/gen/migrate"
	"github.com/AlexAli29/orm/internal/gen/pgintro"
)

// viewPlanInput gathers what the planner needs to prove a view replacement safe.
//
// Two proofs need a database: that it holds the definition this project applied,
// and that nothing has changed it since. Without them the planner refuses to
// replace anything, which is the correct default — so this is not an
// optimisation, it is what makes a legitimate replacement possible at all.
//
// A failure to reach the database is not turned into an error. It leaves the
// input offline, and the planner then refuses replacements with a sentence
// saying it could not check. That is better than failing outright: creating new
// views still works without a server, and it is the same shape the rest of the
// tool has — a check that cannot run says so rather than guessing.
func viewPlanInput(ctx context.Context, dsn string, searchPath []string) migrate.ViewPlanInput {
	if dsn == "" {
		return migrate.ViewPlanInput{}
	}
	conn, err := pgintro.Connect(ctx, dsn)
	if err != nil {
		return migrate.ViewPlanInput{}
	}
	defer func() { _ = conn.Close(context.WithoutCancel(ctx)) }()

	actual, err := pgintro.Canonical(ctx, conn, searchPath)
	if err != nil {
		return migrate.ViewPlanInput{}
	}
	state, err := migrate.ReadViewState(ctx, conn)
	if err != nil {
		return migrate.ViewPlanInput{}
	}
	recorded := make(map[string]string, len(state))
	for k, v := range state {
		recorded[k] = v.Canonical
	}
	return migrate.ViewPlanInput{Actual: actual, Recorded: recorded, Online: true}
}
