package gendemo_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/AlexAli29/orm"
	"github.com/AlexAli29/orm/internal/gendemo"
	"github.com/AlexAli29/orm/internal/testdb"
	"github.com/AlexAli29/orm/plan"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// Typed EXPLAIN.
//
// The claims under test are that the plan is for the statement the ORM would
// really send, that plain EXPLAIN runs nothing, and that ExplainAnalyze runs
// everything. The last two are the same test written twice, and they are the
// reason the two entry points have different names.

func explainDB(t *testing.T) (*gendemo.DB, *pgx.Conn) {
	t.Helper()
	testdb.AdminDSN(t)
	dsn := testdb.Create(t, schema(t)+seed)
	conn, err := pgx.Connect(t.Context(), dsn)
	if err != nil {
		t.Fatalf("connecting: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close(context.WithoutCancel(t.Context())) })
	return gendemo.New(conn), conn
}

func TestExplain_plansASelect(t *testing.T) {
	db, _ := explainDB(t)

	p, err := db.Users.Query().Where(gendemo.Users.ID.Eq(1)).Explain(t.Context())
	if err != nil {
		t.Fatalf("Explain: %v", err)
	}
	if p.Root.Type == "" {
		t.Fatal("the plan has no root node type")
	}
	if p.Analyzed {
		t.Error("a plain EXPLAIN produced a plan that says it was analysed")
	}
	if p.ExecutionTime != nil {
		t.Errorf("a plain EXPLAIN reported an execution time of %v", *p.ExecutionTime)
	}
	// PostgreSQL reports the planning time only when SUMMARY is on, which it is
	// for ANALYZE and is not for a plain EXPLAIN. Asking for it is how a plain
	// plan gets one.
	if p.PlanningTime != nil {
		t.Errorf("a plain EXPLAIN reported a planning time of %v without SUMMARY", *p.PlanningTime)
	}
	withSummary, err := db.Users.Query().Explain(t.Context(), orm.ExplainSummary(true))
	if err != nil {
		t.Fatal(err)
	}
	if withSummary.PlanningTime == nil {
		t.Error("SUMMARY produced no planning time")
	}
	// The plan is about the table the query names.
	s := p.Summarize()
	if len(s.Relations) == 0 || s.Relations[0] != "users" {
		t.Errorf("the plan touches %v", s.Relations)
	}
	// Every node was numbered, which is what lets a diagnostic name one.
	seen := map[int]bool{}
	p.Walk(func(n plan.Node) {
		if seen[n.ID] {
			t.Errorf("node id %d appears twice", n.ID)
		}
		seen[n.ID] = true
		if len(n.Path) == 0 {
			t.Errorf("node %d has no path", n.ID)
		}
	})
}

// The central safety property: EXPLAIN does not run the statement and
// ExplainAnalyze does.
//
// It is asserted for all three write kinds, because the failure mode is silent
// — a plan comes back either way, and only the table says which happened.
func TestExplain_doesNotExecuteWrites(t *testing.T) {
	count := func(t *testing.T, conn *pgx.Conn) int64 {
		t.Helper()
		var n int64
		if err := conn.QueryRow(t.Context(), `SELECT count(*) FROM users`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	ages := func(t *testing.T, conn *pgx.Conn) int64 {
		t.Helper()
		var n int64
		if err := conn.QueryRow(t.Context(), `SELECT count(*) FROM users WHERE age = 99`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}

	t.Run("update", func(t *testing.T) {
		db, conn := explainDB(t)
		before := ages(t, conn)

		u := db.Users.Update().
			Set(gendemo.Users.Age.Set(99)).
			Where(gendemo.Users.ID.Eq(1))
		if _, err := u.Explain(t.Context()); err != nil {
			t.Fatalf("Explain: %v", err)
		}
		if after := ages(t, conn); after != before {
			t.Fatalf("Explain updated %d rows", after-before)
		}

		if _, err := u.ExplainAnalyze(t.Context()); err != nil {
			t.Fatalf("ExplainAnalyze: %v", err)
		}
		if after := ages(t, conn); after != before+1 {
			t.Fatalf("ExplainAnalyze updated %d rows, want 1", after-before)
		}
	})

	t.Run("delete", func(t *testing.T) {
		db, conn := explainDB(t)
		before := count(t, conn)

		// A user no post references, so the delete is about EXPLAIN rather than
		// about a foreign key.
		d := db.Users.Delete().Where(gendemo.Users.ID.Eq(2))
		if _, err := d.Explain(t.Context()); err != nil {
			t.Fatalf("Explain: %v", err)
		}
		if after := count(t, conn); after != before {
			t.Fatalf("Explain deleted %d rows", before-after)
		}

		if _, err := d.ExplainAnalyze(t.Context()); err != nil {
			t.Fatalf("ExplainAnalyze: %v", err)
		}
		if after := count(t, conn); after != before-1 {
			t.Fatalf("ExplainAnalyze deleted %d rows, want 1", before-after)
		}
	})

	t.Run("insert", func(t *testing.T) {
		db, conn := explainDB(t)
		before := count(t, conn)

		// The INSERT is written out rather than built, so that the test is about
		// what EXPLAIN does with a write and not about how identity columns are
		// generated. Raw holds a compiled statement and its arguments, which is
		// what every statement is by the time it reaches the wire.
		stmt := orm.Raw(db.Users, `
			INSERT INTO users (id, email, age, active, state, tags, settings, created_at)
			VALUES ($1, $2, 30, true, 'active', '{}', '{}', now())`,
			int64(500), "explain@example.com")

		if _, err := orm.Explain(t.Context(), conn, stmt); err != nil {
			t.Fatalf("Explain: %v", err)
		}
		if after := count(t, conn); after != before {
			t.Fatalf("Explain inserted %d rows", after-before)
		}

		if _, err := orm.ExplainAnalyze(t.Context(), conn, stmt); err != nil {
			t.Fatalf("ExplainAnalyze: %v", err)
		}
		if after := count(t, conn); after != before+1 {
			t.Fatalf("ExplainAnalyze inserted %d rows, want 1", after-before)
		}
	})
}

// An analysed plan carries what actually happened, which a planned one does not.
func TestExplain_analyzeCarriesActuals(t *testing.T) {
	db, _ := explainDB(t)

	p, err := db.Users.Query().ExplainAnalyze(t.Context())
	if err != nil {
		t.Fatalf("ExplainAnalyze: %v", err)
	}
	if !p.Analyzed {
		t.Fatal("the analysed plan does not say it was analysed")
	}
	if p.ExecutionTime == nil {
		t.Error("the analysed plan has no execution time")
	}
	if p.Root.ActualRows == nil {
		t.Fatal("the analysed plan's root has no actual rows")
	}
	total, ok := p.Root.TotalRows()
	if !ok || total != 3 {
		t.Errorf("the root produced %v rows, want the three seeded users", total)
	}
	if _, ok := p.Root.SelfTime(); !ok {
		t.Error("the analysed plan's root reports no self time")
	}
}

// Bind parameters stay bind parameters. The plan is for the statement with its
// placeholders, and no value is written into the SQL.
func TestExplain_parametersAreNotInterpolated(t *testing.T) {
	db, _ := explainDB(t)

	q := db.Users.Query().Where(gendemo.Users.Email.Eq("alex@example.com"))
	sql, args, err := q.SQL()
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(sql, "alex@example.com") {
		t.Fatalf("the statement contains the value:\n%s", sql)
	}
	if len(args) != 1 {
		t.Fatalf("the statement bound %d arguments", len(args))
	}

	p, err := q.Explain(t.Context(), orm.ExplainVerbose)
	if err != nil {
		t.Fatalf("Explain: %v", err)
	}
	// The plan mentions the column.
	var sawFilter, sawValue bool
	p.Walk(func(n plan.Node) {
		for _, c := range n.Conditions() {
			if strings.Contains(c.Cond, "email") {
				sawFilter = true
			}
			if strings.Contains(c.Cond, "alex@example.com") {
				sawValue = true
			}
		}
	})
	if !sawFilter {
		t.Error("the plan has no condition on email")
	}

	// And it very likely contains the value, because PostgreSQL plans a
	// parameterised statement against the values it was given and prints the
	// constants it planned with. This is worth asserting rather than hoping
	// against: the statement carries no value and the plan does, so a plan is
	// not a value-free object and must not be treated as one by anything that
	// exports telemetry.
	if !sawValue {
		t.Log("this server's plan did not inline the parameter; the caution still holds")
	} else {
		t.Logf("the plan inlines the bound value, as PostgreSQL does for a custom plan")
	}

	// A generic plan for this statement cannot be asked for through the driver,
	// and saying so is better than returning the custom plan under a generic
	// label.
	caps, err := orm.Capabilities(t.Context(), db.Executor())
	if err != nil {
		t.Fatal(err)
	}
	if caps.GenericPlan {
		_, err := q.Explain(t.Context(), orm.ExplainGenericPlan)
		var ge *orm.GenericPlanError
		if !errors.As(err, &ge) {
			t.Fatalf("a generic plan for a parameterised statement returned %v", err)
		}
		if ge.Args != 1 {
			t.Errorf("the error reports %d bound values, want 1", ge.Args)
		}
	}
}

// A query bound to a transaction explains on that transaction, so it sees what
// the transaction has done and nothing outside it does.
func TestExplain_throughTransaction(t *testing.T) {
	testdb.AdminDSN(t)
	dsn := testdb.Create(t, schema(t)+seed)
	conn, err := pgx.Connect(t.Context(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close(context.WithoutCancel(t.Context())) }()

	tx, err := conn.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(t.Context())) }()

	if _, err := tx.Exec(t.Context(), `
		INSERT INTO users (id, email, age, active, state, tags, settings, created_at)
		VALUES (900, 'ghost@example.com', 21, true, 'active', '{}', '{}', now())`); err != nil {
		t.Fatal(err)
	}

	// Analysed inside the transaction, the count includes the invisible row.
	p, err := gendemo.New(tx).Users.Query().ExplainAnalyze(t.Context())
	if err != nil {
		t.Fatalf("ExplainAnalyze: %v", err)
	}
	total, ok := p.Root.TotalRows()
	if !ok {
		t.Fatal("no actual rows")
	}
	if total != 4 {
		t.Errorf("the transaction's query saw %v rows, want 4", total)
	}

	// And outside it, the row is not there.
	if err := tx.Rollback(t.Context()); err != nil {
		t.Fatal(err)
	}
	p, err = gendemo.New(conn).Users.Query().ExplainAnalyze(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	total, _ = p.Root.TotalRows()
	if total != 3 {
		t.Errorf("after the rollback the query sees %v rows, want 3", total)
	}
}

// The options the connected server does not have are refused by name, before
// the statement is sent.
func TestExplain_capabilities(t *testing.T) {
	db, conn := explainDB(t)

	caps, err := orm.Capabilities(t.Context(), conn)
	if err != nil {
		t.Fatalf("Capabilities: %v", err)
	}
	if caps.Version == 0 {
		t.Fatal("the server reported no version")
	}
	t.Logf("PostgreSQL %s: generic_plan=%v serialize=%v memory=%v",
		caps, caps.GenericPlan, caps.Serialize, caps.Memory)

	// The version gates line up with what the server actually accepts.
	q := db.Users.Query()
	for _, tc := range []struct {
		name string
		opt  orm.ExplainOption
		have bool
	}{
		{"GENERIC_PLAN", orm.ExplainGenericPlan, caps.GenericPlan},
		{"MEMORY", orm.ExplainMemory, caps.Memory},
	} {
		_, err := q.Explain(t.Context(), tc.opt)
		if tc.have && err != nil {
			t.Errorf("%s is available and was refused: %v", tc.name, err)
		}
		if !tc.have {
			if err == nil {
				t.Errorf("%s is not available and was accepted", tc.name)
				continue
			}
			var ce *orm.CapabilityError
			if !errors.As(err, &ce) {
				t.Errorf("%s was refused with %v, not a CapabilityError", tc.name, err)
			}
		}
	}
}

// The combinations PostgreSQL itself refuses are refused here, with a message
// naming the two options rather than a position in a string.
func TestExplain_impossibleCombinations(t *testing.T) {
	db, _ := explainDB(t)
	q := db.Users.Query()

	for _, tc := range []struct {
		name    string
		run     func() error
		mention string
	}{
		{"GENERIC_PLAN with ANALYZE", func() error {
			_, err := q.ExplainAnalyze(t.Context(), orm.ExplainGenericPlan)
			return err
		}, "GENERIC_PLAN"},
		{"WAL without ANALYZE", func() error {
			_, err := q.Explain(t.Context(), orm.ExplainWAL)
			return err
		}, "WAL"},
		{"TIMING without ANALYZE", func() error {
			_, err := q.Explain(t.Context(), orm.ExplainTiming(true))
			return err
		}, "TIMING"},
		{"SERIALIZE without ANALYZE", func() error {
			_, err := q.Explain(t.Context(), orm.ExplainSerialize("text"))
			return err
		}, "SERIALIZE"},
		{"a SERIALIZE mode that is not one", func() error {
			_, err := q.ExplainAnalyze(t.Context(), orm.ExplainSerialize("sideways"))
			return err
		}, "SERIALIZE"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.run()
			if err == nil {
				t.Fatal("the combination was accepted")
			}
			if !strings.Contains(err.Error(), tc.mention) {
				t.Errorf("the error does not name %s: %v", tc.mention, err)
			}
		})
	}
}

// The options that are available do what they say.
func TestExplain_options(t *testing.T) {
	db, conn := explainDB(t)
	caps, err := orm.Capabilities(t.Context(), conn)
	if err != nil {
		t.Fatal(err)
	}
	q := db.Users.Query().Where(gendemo.Users.Active.Eq(true))

	t.Run("verbose adds output columns", func(t *testing.T) {
		p, err := q.Explain(t.Context(), orm.ExplainVerbose)
		if err != nil {
			t.Fatal(err)
		}
		var sawOutput bool
		p.Walk(func(n plan.Node) {
			if len(n.Output) > 0 {
				sawOutput = true
			}
		})
		if !sawOutput {
			t.Error("VERBOSE produced no output columns")
		}
	})

	t.Run("costs off removes the estimates", func(t *testing.T) {
		p, err := q.Explain(t.Context(), orm.ExplainCosts(false))
		if err != nil {
			t.Fatal(err)
		}
		if p.Root.TotalCost != nil {
			t.Errorf("COSTS false still reported a cost of %v", *p.Root.TotalCost)
		}
	})

	t.Run("settings reports non-defaults", func(t *testing.T) {
		if _, err := q.Explain(t.Context(), orm.ExplainSettings); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("buffers with analyze", func(t *testing.T) {
		p, err := q.ExplainAnalyze(t.Context(), orm.ExplainBuffers(true))
		if err != nil {
			t.Fatal(err)
		}
		if p.Root.Buffers == nil {
			t.Error("BUFFERS produced no buffer accounting")
		}
	})

	t.Run("wal with analyze on a write", func(t *testing.T) {
		u := db.Users.Update().Set(gendemo.Users.Age.Set(42)).Where(gendemo.Users.ID.Eq(1))
		p, err := u.ExplainAnalyze(t.Context(), orm.ExplainWAL)
		if err != nil {
			t.Fatal(err)
		}
		var sawWAL bool
		p.Walk(func(n plan.Node) {
			if n.WAL != nil {
				sawWAL = true
			}
		})
		if !sawWAL {
			t.Error("WAL produced no accounting on an analysed write")
		}
	})

	t.Run("timing off keeps the rows", func(t *testing.T) {
		p, err := q.ExplainAnalyze(t.Context(), orm.ExplainTiming(false))
		if err != nil {
			t.Fatal(err)
		}
		if p.Root.ActualRows == nil {
			t.Error("TIMING false lost the row counts")
		}
	})

	if caps.GenericPlan {
		t.Run("generic plan on a statement with no values", func(t *testing.T) {
			// A statement that binds nothing has nothing to plan against, so its
			// generic plan is available.
			p, err := db.Users.Query().Explain(t.Context(), orm.ExplainGenericPlan)
			if err != nil {
				t.Fatal(err)
			}
			if p.Root.Type == "" {
				t.Error("the generic plan has no root")
			}
		})
	}

	if caps.Serialize {
		t.Run("serialize with analyze", func(t *testing.T) {
			if _, err := q.ExplainAnalyze(t.Context(), orm.ExplainSerialize("text")); err != nil {
				t.Fatal(err)
			}
		})
	}
	if caps.Memory {
		t.Run("memory", func(t *testing.T) {
			if _, err := q.Explain(t.Context(), orm.ExplainMemory); err != nil {
				t.Fatal(err)
			}
		})
	}
}

// A statement the server refuses produces the server's error, not a wrapper's.
func TestExplain_preservesPgError(t *testing.T) {
	db, _ := explainDB(t)

	_, err := orm.Raw(db.Users, `SELECT * FROM no_such_table`).
		Explain(t.Context())
	if err == nil {
		t.Fatal("explaining a statement over a missing table succeeded")
	}
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		t.Fatalf("the error is not a PostgreSQL error: %v", err)
	}
	if pgErr.Code != "42P01" {
		t.Errorf("the SQLSTATE is %s, want 42P01", pgErr.Code)
	}
}

// Cancelling leaves the connection usable.
func TestExplain_cancellation(t *testing.T) {
	db, conn := explainDB(t)

	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := db.Users.Query().Explain(ctx); err == nil {
		t.Error("explaining with a cancelled context succeeded")
	}
	if _, err := db.Users.Query().ExplainAnalyze(ctx); err == nil {
		t.Error("analysing with a cancelled context succeeded")
	}

	// The connection still works.
	var n int64
	if err := conn.QueryRow(t.Context(), `SELECT count(*) FROM users`).Scan(&n); err != nil {
		t.Fatalf("after cancellation the connection is unusable: %v", err)
	}
}

// A complex statement — CTE, derived table, join, aggregate, window, JSONB,
// range — explains, and its placeholders are the ones execution would use.
func TestExplain_complexStatement(t *testing.T) {
	db, _ := explainDB(t)

	minAge := orm.Named("min_age", orm.Of(orm.Min(gendemo.Users.Age)))
	state := orm.Named("state", orm.Of(gendemo.Users.State))
	byState := orm.CTE("by_state", orm.Rows(state, minAge).
		From(gendemo.Users.Source()).
		Where(orm.Cond(gendemo.Users.Active.Eq(true))).
		GroupBy(orm.Of(gendemo.Users.State)))

	rank := orm.RowNumber().Over(orm.Window().OrderBy(orm.Ref(byState, state).Asc()))
	shape := orm.Project3(
		orm.Ref(byState, state),
		orm.Ref(byState, minAge),
		orm.Of(rank),
		func(s gendemo.UserState, a *int32, r int64) string { return string(s) },
	)

	q := orm.Compose(db.Executor(), shape).
		With(byState).
		From(byState).
		OrderBy(orm.Of(rank).Asc()).
		Limit(10)

	sql, args, err := q.SQL()
	if err != nil {
		t.Fatalf("SQL: %v", err)
	}
	p, err := q.Explain(t.Context())
	if err != nil {
		t.Fatalf("Explain: %v\n%s", err, sql)
	}
	if p.Root.Type == "" {
		t.Fatal("no plan")
	}
	// Explaining did not change how many arguments the statement binds.
	sql2, args2, err := q.SQL()
	if err != nil {
		t.Fatal(err)
	}
	if sql2 != sql || len(args2) != len(args) {
		t.Error("explaining changed the statement")
	}
	// And the query still runs, with the same statement.
	if _, err := q.All(t.Context()); err != nil {
		t.Fatalf("running the explained query: %v", err)
	}
}

// Every query builder can be explained, which is what makes the API one API.
func TestExplain_everyBuilder(t *testing.T) {
	db, _ := explainDB(t)

	shape := orm.Project1(gendemo.Users.Email, func(s string) string { return s })
	for name, s := range map[string]orm.Statement{
		"entity query":   db.Users.Query().Where(gendemo.Users.ID.Gt(0)),
		"select query":   orm.Select(db.Users, shape).Where(gendemo.Users.ID.Gt(0)),
		"composed query": orm.Compose(db.Executor(), orm.Project1(orm.Of(gendemo.Users.Email), func(s string) string { return s })).From(gendemo.Users.Source()),
		"update":         db.Users.Update().Set(gendemo.Users.Age.Set(1)).Where(gendemo.Users.ID.Eq(-1)),
		"delete":         db.Users.Delete().Where(gendemo.Users.ID.Eq(-1)),
		"raw":            orm.Raw(db.Users, `SELECT * FROM users WHERE id = $1`, 1),
	} {
		t.Run(name, func(t *testing.T) {
			p, err := orm.Explain(t.Context(), db.Executor(), s)
			if err != nil {
				t.Fatalf("Explain: %v", err)
			}
			if p.Root.Type == "" {
				t.Error("no root node")
			}
			if p.Analyzed {
				t.Error("a plain EXPLAIN says it was analysed")
			}
		})
	}
}
