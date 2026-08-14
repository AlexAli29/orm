package orm

import (
	"context"
	"fmt"
	"strings"

	"github.com/AlexAli29/orm/observe"
	"github.com/AlexAli29/orm/plan"
)

// Explaining a statement.
//
// EXPLAIN wraps a statement rather than replacing it, so this compiles the
// query exactly once, through the same writer, with the same placeholders and
// the same arguments, and puts EXPLAIN in front of the result. There is no
// second compiler and no second path: a plan for a statement this package could
// not run would be a plan for a statement nobody has.
//
// The distinction that matters most here is between the two entry points.
// [Explain] asks the planner what it would do and runs nothing. [ExplainAnalyze]
// runs the statement — every row it would insert is inserted, every row it would
// delete is deleted — and it is a separate function with a separate name so that
// nobody reaches it by passing a flag.

// Statement is anything this package compiles to SQL.
//
// Every query and write builder satisfies it, which is what lets [Explain] take
// any of them without a type switch and without knowing what they are.
type Statement interface {
	SQL() (string, []any, error)
}

// Explain returns PostgreSQL's plan for the statement without running it.
//
// The statement is compiled once, through the same writer that would execute it,
// and wrapped. The parameters are the statement's own — nothing is interpolated,
// so the plan is for the statement as it would really be sent.
//
// It does not execute the statement. For an INSERT, UPDATE or DELETE, no row
// changes; PostgreSQL plans the write and discards the plan. That is the whole
// difference between this and [ExplainAnalyze], and a permanent test asserts it
// for all three.
func Explain(ctx context.Context, ex Executor, s Statement, opts ...ExplainOption) (*plan.Plan, error) {
	return explain(ctx, ex, s, false, opts)
}

// ExplainAnalyze runs the statement and returns the plan with what actually
// happened in it.
//
// It executes. An INSERT inserts, an UPDATE updates, a DELETE deletes, a
// volatile function runs, a locking query takes its locks. The name says so
// because there is no way for a library to make executing a statement safe:
//
// A transaction can undo what the database did, and it is the right tool when
// what is wanted is the plan without the change:
//
//	tx, err := pool.Begin(ctx)
//	if err != nil {
//	    return err
//	}
//	defer tx.Rollback(ctx)
//	p, err := orm.ExplainAnalyze(ctx, tx, stmt)
//
// What a rollback cannot undo is anything the statement did outside the
// database — a function that sent a message, an extension that wrote a file, a
// trigger that called out. This package does not wrap statements in a
// transaction on your behalf and does not describe any wrapping as safe,
// because the word would be untrue for exactly the cases where it matters.
//
// Nothing inside this package calls it. The diagnostics use
// [Explain], and taking runtime measurements is always something a caller asked
// for by name.
func ExplainAnalyze(ctx context.Context, ex Executor, s Statement, opts ...ExplainOption) (*plan.Plan, error) {
	return explain(ctx, ex, s, true, opts)
}

func explain(ctx context.Context, ex Executor, s Statement, analyze bool, opts []ExplainOption) (*plan.Plan, error) {
	if ex == nil {
		return nil, fmt.Errorf("orm: explaining needs an executor")
	}
	if s == nil {
		return nil, fmt.Errorf("orm: explaining needs a statement")
	}

	cfg := &explainConfig{analyze: analyze}
	for _, o := range opts {
		if o == nil {
			continue
		}
		if err := o.applyExplain(cfg); err != nil {
			return nil, err
		}
	}

	// The statement is compiled through its own builder, so every check it
	// would have made — scope, nullability, assignment — is made here too. A
	// query that will not run does not get a plan.
	sql, args, err := s.SQL()
	if err != nil {
		return nil, err
	}

	caps, err := Capabilities(ctx, ex)
	if err != nil {
		return nil, err
	}
	options, err := cfg.validate(caps)
	if err != nil {
		return nil, err
	}

	// A generic plan is the plan PostgreSQL makes without the parameter values,
	// so the statement has to reach the server with its placeholders unbound.
	// The driver cannot do that: pgx describes a parameterised statement and
	// refuses a count mismatch on the extended protocol, and substitutes the
	// placeholders itself on the simple one. Either way a statement with
	// arguments cannot be sent without them.
	//
	// Sending the values anyway would return the plan made with them, which is a
	// real plan and is the other one — so this refuses rather than returning a
	// custom plan under a generic label.
	if cfg.generic && len(args) > 0 {
		return nil, &GenericPlanError{Args: len(args)}
	}

	// EXPLAIN's own option list is syntax and is built from the closed set
	// above; the statement is the statement, and its placeholders keep their
	// numbers because nothing is prepended to the parameter list.
	stmt := "EXPLAIN (" + strings.Join(options, ", ") + ") " + strings.TrimSuffix(sql, ";")

	// A generic plan is the plan PostgreSQL makes without the parameter values,
	// so the values are not sent. Sending them would get a plan made with them —
	// which is a real plan, and is the other one.
	//
	// It is also the only plan guaranteed to contain no value: PostgreSQL prints
	// the constants it planned with, so an ordinary plan's conditions read
	// (email = 'someone@example.com') even though the statement carried a
	// placeholder. Nothing here changes that; it is the server's output, and it
	// is why a plan is not something to put in telemetry by default.
	// Explaining is an operation a tracer should see, so it emits the ops the
	// observability model declares for it. What the event carries is the
	// statement's own SQL — placeholders and all — and never the plan: the plan
	// is the server's output and contains the constants it planned with, which
	// is exactly what telemetry must not receive by default.
	op := observe.OpExplain
	if analyze {
		op = observe.OpExplainAnalyze
	}
	ctx, sp := startSpan(ctx, ex, observe.StartEvent{
		Op:   op,
		SQL:  sql,
		Args: len(args),
	})

	rows, err := ex.Query(ctx, stmt, args...)
	if err != nil {
		// The server's error is the error. A statement EXPLAIN refuses is a
		// statement the caller wrote, and PostgreSQL says why better than a
		// wrapper could.
		sp.end(err, 0, false)
		return nil, err
	}
	defer rows.Close()

	var raw []byte
	if !rows.Next() {
		err := rows.Err()
		if err == nil {
			err = fmt.Errorf("orm: EXPLAIN returned no rows")
		}
		sp.end(err, 0, false)
		return nil, err
	}
	if err := rows.Scan(&raw); err != nil {
		err = fmt.Errorf("orm: reading the plan: %w", err)
		sp.end(err, 0, false)
		return nil, err
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		sp.end(err, 0, false)
		return nil, err
	}

	p, err := plan.Parse(raw)
	if err != nil {
		sp.end(err, 0, false)
		return nil, err
	}
	sp.end(nil, 1, true)
	return p, nil
}

// The convenience methods.
//
// Each is the free function with the query's own executor filled in, which is
// the form a caller writing a query wants: the executor is already decided.

// Explain returns PostgreSQL's plan for this query without running it.
func (q *Query[E]) Explain(ctx context.Context, opts ...ExplainOption) (*plan.Plan, error) {
	return Explain(ctx, q.repo.ex, q, opts...)
}

// ExplainAnalyze runs the query and returns the plan with what happened in it.
func (q *Query[E]) ExplainAnalyze(ctx context.Context, opts ...ExplainOption) (*plan.Plan, error) {
	return ExplainAnalyze(ctx, q.repo.ex, q, opts...)
}

// Explain returns PostgreSQL's plan for this query without running it.
func (q *SelectQuery[E, R]) Explain(ctx context.Context, opts ...ExplainOption) (*plan.Plan, error) {
	return Explain(ctx, q.repo.ex, q, opts...)
}

// ExplainAnalyze runs the query and returns the plan with what happened in it.
func (q *SelectQuery[E, R]) ExplainAnalyze(ctx context.Context, opts ...ExplainOption) (*plan.Plan, error) {
	return ExplainAnalyze(ctx, q.repo.ex, q, opts...)
}

// Explain returns PostgreSQL's plan for this query without running it.
func (q *ComposedQuery[R]) Explain(ctx context.Context, opts ...ExplainOption) (*plan.Plan, error) {
	return Explain(ctx, q.ex, q, opts...)
}

// ExplainAnalyze runs the query and returns the plan with what happened in it.
func (q *ComposedQuery[R]) ExplainAnalyze(ctx context.Context, opts ...ExplainOption) (*plan.Plan, error) {
	return ExplainAnalyze(ctx, q.ex, q, opts...)
}

// Explain returns PostgreSQL's plan for this statement without running it.
//
// No row is updated: PostgreSQL plans the write and throws the plan away.
func (u *Update[E]) Explain(ctx context.Context, opts ...ExplainOption) (*plan.Plan, error) {
	return Explain(ctx, u.repo.ex, u, opts...)
}

// ExplainAnalyze runs the update and returns the plan.
//
// The rows are updated. Run it inside a transaction you roll back if that is
// not what was wanted.
func (u *Update[E]) ExplainAnalyze(ctx context.Context, opts ...ExplainOption) (*plan.Plan, error) {
	return ExplainAnalyze(ctx, u.repo.ex, u, opts...)
}

// Explain returns PostgreSQL's plan for this statement without running it.
//
// No row is deleted.
func (d *Delete[E]) Explain(ctx context.Context, opts ...ExplainOption) (*plan.Plan, error) {
	return Explain(ctx, d.repo.ex, d, opts...)
}

// ExplainAnalyze runs the delete and returns the plan.
//
// The rows are deleted.
func (d *Delete[E]) ExplainAnalyze(ctx context.Context, opts ...ExplainOption) (*plan.Plan, error) {
	return ExplainAnalyze(ctx, d.repo.ex, d, opts...)
}

// Explain returns PostgreSQL's plan for this raw query without running it.
//
// The SQL is the caller's, so the plan is for exactly what they wrote. What is
// unavailable here is everything downstream that reads the ORM's own structure:
// a raw statement has no AST, so the static diagnostics have much less
// to work with, and they say so rather than guessing from the text.
func (q *RawQuery[E]) Explain(ctx context.Context, opts ...ExplainOption) (*plan.Plan, error) {
	return Explain(ctx, q.repo.ex, q, opts...)
}

// ExplainAnalyze runs the raw query and returns the plan.
//
// The statement runs, whatever it is.
func (q *RawQuery[E]) ExplainAnalyze(ctx context.Context, opts ...ExplainOption) (*plan.Plan, error) {
	return ExplainAnalyze(ctx, q.repo.ex, q, opts...)
}

// GenericPlanError reports a generic plan asked for on a statement that binds
// values.
//
// GENERIC_PLAN asks PostgreSQL what it would do without knowing the parameter
// values, so the statement has to arrive with its placeholders unbound — and the
// driver has no way to send one. Returning the plan made with the values would
// be returning a custom plan and calling it generic, which is the one thing this
// option exists to distinguish.
//
// The generic plan for such a statement is obtainable outside this package, over
// a raw connection:
//
//	sql, _, _ := q.SQL()
//	res, err := conn.PgConn().Exec(ctx, "EXPLAIN (GENERIC_PLAN, FORMAT JSON) "+sql).ReadAll()
//
// and the JSON it returns parses with plan.Parse.
type GenericPlanError struct {
	// Args is how many values the statement binds.
	Args int
}

// Error explains that a generic plan was asked for a statement that binds
// values, and says how to obtain one anyway.
func (e *GenericPlanError) Error() string {
	return fmt.Sprintf("orm: GENERIC_PLAN asks for the plan PostgreSQL would make without the"+
		" parameter values, and this statement binds %d of them;"+
		" the driver cannot send a statement with unbound placeholders,"+
		" so a generic plan for it has to be asked for over a raw connection", e.Args)
}
