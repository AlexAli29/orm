package uuidcompat_test

// The programs the staged module runs.
//
// They are sources rather than packages of this module because they have to be
// compiled against the staged copy's generated code — the whole question is
// what a particular generation of the descriptor does, and a package compiled
// here would be compiled against this module's.

// refreshProbe attempts a concurrent refresh and reports what happened,
// including how many statements reached the server.
//
// The count is the only thing that distinguishes the two failure modes from
// outside. Both produce an error; one produced it without asking.
const refreshProbe = `package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"sync/atomic"

	"example.com/uuidcompat/domain"
	"github.com/AlexAli29/orm"
	"github.com/AlexAli29/orm/observe"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// counter records every statement the ORM starts whose SQL is a refresh.
type counter struct{ n atomic.Int64 }

func (c *counter) Start(ctx context.Context, e observe.StartEvent) context.Context {
	if strings.Contains(strings.ToUpper(e.SQL), "REFRESH") {
		c.n.Add(1)
	}
	return ctx
}

func (c *counter) End(context.Context, observe.EndEvent) {}

type result struct {
	Err        string ` + "`json:\"err\"`" + `
	Statements int    ` + "`json:\"statements\"`" + `
	SQLState   string ` + "`json:\"sqlstate\"`" + `
	IsPgError  bool   ` + "`json:\"is_pg_error\"`" + `
}

func main() {
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, os.Getenv("UUIDCOMPAT_DSN"))
	if err != nil {
		panic(err)
	}
	defer pool.Close()

	c := &counter{}
	db := domain.New(orm.Traced(pool, c))

	var out result
	if err := db.UserSummaries.Refresh(ctx, orm.Concurrently()); err != nil {
		out.Err = err.Error()
		var pge *pgconn.PgError
		if errors.As(err, &pge) {
			out.IsPgError = true
			out.SQLState = pge.Code
		}
	}
	out.Statements = int(c.n.Load())
	_ = json.NewEncoder(os.Stdout).Encode(out)
}
`

// eligibilityProbe asks PostgreSQL whether the materialized view has an index
// a concurrent refresh could use.
//
// This is the server's opinion and not the generator's, which is what makes it
// usable as the precondition for a claim about the generator being wrong.
const eligibilityProbe = `package main

import (
	"context"
	"fmt"
	"os"

	"github.com/jackc/pgx/v5"
)

func main() {
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, os.Getenv("UUIDCOMPAT_DSN"))
	if err != nil {
		panic(err)
	}
	defer conn.Close(ctx)

	// Unique, not partial, and over plain columns: PostgreSQL's own conditions
	// for REFRESH ... CONCURRENTLY.
	var n int
	err = conn.QueryRow(ctx, ` + "`" + `
		SELECT count(*)
		  FROM pg_index i
		 WHERE i.indrelid = 'public.user_summaries'::regclass
		   AND i.indisunique
		   AND i.indpred IS NULL
		   AND i.indexprs IS NULL` + "`" + `).Scan(&n)
	if err != nil {
		panic(err)
	}
	if n > 0 {
		fmt.Println("yes")
		return
	}
	fmt.Println("no")
}
`

// typeProbe prints format_type for one column of the staged database.
const typeProbe = `package main

import (
	"context"
	"fmt"
	"os"

	"github.com/jackc/pgx/v5"
)

func main() {
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, os.Getenv("UUIDCOMPAT_DSN"))
	if err != nil {
		panic(err)
	}
	defer conn.Close(ctx)

	var t string
	err = conn.QueryRow(ctx,
		"SELECT format_type(a.atttypid, a.atttypmod) FROM pg_attribute a "+
			"WHERE a.attrelid = '__REL__'::regclass AND a.attname = '__COL__'").Scan(&t)
	if err != nil {
		panic(err)
	}
	fmt.Println(t)
}
`

// execProbe runs one statement against the staged database.
const execProbe = `package main

import (
	"context"
	"os"

	"github.com/jackc/pgx/v5"
)

func main() {
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, os.Getenv("UUIDCOMPAT_DSN"))
	if err != nil {
		panic(err)
	}
	defer conn.Close(ctx)
	if _, err := conn.Exec(ctx, ` + "`__SQL__`" + `); err != nil {
		panic(err)
	}
}
`

// majorProbe prints the server's major version number.
const majorProbe = `package main

import (
	"context"
	"fmt"
	"os"
	"strconv"

	"github.com/jackc/pgx/v5"
)

func main() {
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, os.Getenv("UUIDCOMPAT_DSN"))
	if err != nil {
		panic(err)
	}
	defer conn.Close(ctx)

	// SHOW returns text, so it is read as text and converted here rather than
	// scanned into an int that pgx would refuse.
	var raw string
	if err := conn.QueryRow(ctx, "SHOW server_version_num").Scan(&raw); err != nil {
		panic(err)
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		panic(err)
	}
	fmt.Println(strconv.Itoa(n / 10000))
}
`

// workloadProbe is the whole uuid surface, run through the staged module's own
// generated code against whichever server it is pointed at.
//
// Every step is a real statement. A major that only answered catalog queries
// would prove that it has a catalog.
const workloadProbe = `package main

import (
	"context"
	"fmt"
	"os"

	"example.com/uuidcompat/domain"
	"example.com/uuidcompat/tenanta"
	"example.com/uuidcompat/tenantb"
	"github.com/AlexAli29/orm"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

func check(what string, err error) {
	if err != nil {
		fmt.Printf("%s: %v\n", what, err)
		os.Exit(1)
	}
}

func main() {
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, os.Getenv("UUIDCOMPAT_DSN"))
	check("connecting", err)
	defer pool.Close()

	db := domain.New(pool)

	// A uuid primary key, a uuid array, a nullable uuid.
	alice, bob := uuid.New(), uuid.New()
	opt := uuid.New()
	_, err = db.Users.Insert(ctx, domain.User{
		ID: alice, Email: "alice@example.com", ExternalID: uuid.New(),
		OptionalID: &opt, Tags: []uuid.UUID{uuid.New(), uuid.New()},
	})
	check("insert alice", err)
	_, err = db.Users.Insert(ctx, domain.User{
		ID: bob, Email: "bob@example.com", ExternalID: uuid.New(), Tags: []uuid.UUID{},
	})
	check("insert bob", err)

	// A uuid foreign key.
	_, err = db.Orders.Insert(ctx, domain.Order{
		ID: uuid.New(), UserID: alice, Label: "one",
	})
	check("insert order", err)

	// Read it back by uuid, and through the relation.
	got, err := db.Users.Query().Where(domain.Users.ID.Eq(alice)).
		With(domain.Users.Orders).One(ctx)
	check("query alice", err)
	if got.ID != alice || len(got.Tags) != 2 || got.OptionalID == nil || *got.OptionalID != opt {
		fmt.Println("alice did not round-trip")
		os.Exit(1)
	}
	if got.Orders.Len() != 1 {
		fmt.Println("the uuid foreign key did not load the relation")
		os.Exit(1)
	}

	// The nullable uuid is NULL for bob and not for alice.
	n, err := db.Users.Query().Where(domain.Users.OptionalID.IsNull()).Count(ctx)
	check("counting nulls", err)
	if n != 1 {
		fmt.Printf("%d rows have a NULL nullable uuid, want 1\n", n)
		os.Exit(1)
	}

	// Source-induced nullability across an outer join.
	type row struct {
		u uuid.UUID
		o *uuid.UUID
	}
	shape := orm.Project2(orm.Of(domain.Users.ID), orm.Opt(domain.Orders.ID),
		func(u uuid.UUID, o *uuid.UUID) row { return row{u: u, o: o} })
	rows, err := orm.Compose(pool, shape).
		From(domain.Users.Source()).
		LeftJoin(domain.Orders.Source(), orm.Eq(domain.Orders.UserID, domain.Users.ID)).
		All(ctx)
	check("outer join", err)
	var absent int
	for _, r := range rows {
		if r.o == nil {
			absent++
		}
	}
	if absent != 1 {
		fmt.Printf("%d outer-join rows are absent, want 1\n", absent)
		os.Exit(1)
	}

	// A projection with the int8 aggregate beside the uuid.
	grouped := orm.Project2(domain.Orders.UserID, orm.Count[domain.Order](),
		func(u uuid.UUID, c int64) row { return row{u: u} })
	if _, err := orm.Select(db.Orders, grouped).GroupBy(domain.Orders.UserID).All(ctx); err != nil {
		check("grouping", err)
	}

	// The view.
	vs, err := db.UserOrders.Query().Where(domain.UserOrders.UserID.Eq(alice)).All(ctx)
	check("view", err)
	if len(vs) != 1 {
		fmt.Printf("the view returned %d rows, want 1\n", len(vs))
		os.Exit(1)
	}

	// The materialized view, both refresh forms, and its unique uuid index.
	check("refresh", db.UserSummaries.Refresh(ctx))
	check("refresh concurrently", db.UserSummaries.Refresh(ctx, orm.Concurrently()))
	ms, err := db.UserSummaries.Query().Where(domain.UserSummaries.UserID.Eq(alice)).All(ctx)
	check("materialized view", err)
	if len(ms) != 1 || ms[0].Orders != 1 {
		fmt.Printf("the materialized view returned %+v\n", ms)
		os.Exit(1)
	}

	// A domain over uuid, and a value the server generates.
	tok, err := db.Tokens.Insert(ctx,
		domain.Token{ID: uuid.New(), TenantID: uuid.New()},
		orm.Default(domain.Tokens.Value))
	check("token", err)
	if tok.Value == uuid.Nil {
		fmt.Println("the server default produced the zero UUID")
		os.Exit(1)
	}

	// Cross-schema, same basename.
	a, b := tenanta.New(pool), tenantb.New(pool)
	ida, idb := uuid.New(), uuid.New()
	_, err = a.Users.Insert(ctx, tenanta.User{ID: ida, Label: "a"})
	check("schema_a", err)
	_, err = b.Users.Insert(ctx, tenantb.User{ID: idb, Label: "b"})
	check("schema_b", err)
	cross, err := a.Users.Query().Where(tenanta.Users.ID.Eq(idb)).All(ctx)
	check("cross-schema query", err)
	if len(cross) != 0 {
		fmt.Println("schema_a returned a row for schema_b's uuid")
		os.Exit(1)
	}

	fmt.Println("ok")
}
`
