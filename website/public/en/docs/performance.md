# Performance

> Reading plans, fingerprinting statements, and the work the ORM does not do.

Source: https://ormgo.vercel.app/en/docs/performance/
Symbols: https://ormgo.vercel.app/api/orm.txt — the generated list of every exported name.

---
## What this is not

There is no index advisor and no tuning advice. PostgreSQL plans, and what to change about a schema or a server needs a whole workload rather than one statement. What is here reports; it never recommends.

## Explain

```go
plan, err := q.Explain(ctx)        // EXPLAIN — does not run the statement
plan, err := q.ExplainAnalyze(ctx) // EXPLAIN ANALYZE — runs it, to measure it
```

The names differ because the behaviours differ dangerously. `ExplainAnalyze` on a `DELETE` deletes.

```go
plan, _ := q.Explain(ctx)
fmt.Println(plan.TotalCost, plan.PlanRows)
for _, node := range plan.Walk() {
    if node.Type == "Seq Scan" && node.PlanRows > 10000 {
        // a big sequential scan, reported rather than diagnosed
    }
}
```

## Diagnostics without a database

```go
report, err := q.Diagnostics()
```

Structure only — how many joins, whether a `LIMIT` is present, whether the statement is correlated. It needs no connection, so it runs in a unit test.

## Fingerprints

A fingerprint identifies a statement's *shape*, independently of its values:

```go
fp, err := q.Fingerprint()
// v1:9f2c... — the same for every set of arguments
```

Two statements differing only in bind values fingerprint the same; two differing in `LIMIT` do not, because a small limit is what makes the planner prefer an index it would otherwise ignore. Grouping by "is the same statement" is worth more than grouping by "looks similar".

## The performance report

```go
report, err := q.PerformanceReport(ctx)
```

Plan, shape and fingerprint together, always with a plain `EXPLAIN` and never an `ANALYZE`.

## Things that are fast because of how the API is shaped

**Relations are batched.** The statement count follows the shape of the tree you asked for, never the number of rows. There is no N+1 to find because there is no lazy loading to cause one.

**Scanning does no reflection.** A projection scans into N typed locals with one `Scan` and one call. Entities scan through generated metadata.

**Descriptors are shared, not copied.** They are read-only, so every query over a table reuses one set.

**`Count` and `Exists` select a constant.** A row per match and no values from it, because asking for the columns would make the server fetch and decode data nobody reads.

**COPY exists for bulk.** `CopyFrom` and `CopyFromSeq` are an order of magnitude faster than `INSERT` for loading, and the streaming form never holds the whole batch.

## Measuring

The repository has benchmarks that are compiled and run in CI, so a regression in the work between the caller and pgx shows up as a build failure rather than as nothing:

```bash
go test -run '^$' -bench . -benchtime 10x ./...
```

## Worked examples

### Reading a plan before trusting a query

```go
q := db.Orders.Query().
    Where(Orders.CustomerID.Eq(id)).
    OrderBy(Orders.PlacedAt.Desc()).
    Limit(20)

plan, err := q.Explain(ctx)
fmt.Println(plan.TotalCost, plan.PlanRows)
```

`Explain` does not run the statement. `ExplainAnalyze` does, which is why they
have different names — running one on a `DELETE` deletes.

### Grouping slow queries by shape

```go
fp, err := q.Fingerprint()
metrics.Observe(fp.String(), elapsed)
```

Two calls with different customer ids fingerprint the same; the same query with a
different `LIMIT` does not, because a small limit is what makes the planner
choose an index it would otherwise skip.

### Checking a query's shape in a unit test

```go
report, err := q.Diagnostics()   // no database needed
if report.Joins > 3 {
    t.Errorf("this grew a join nobody meant to add")
}
```

### Counting statements rather than guessing

```go
db := domain.New(orm.Traced(pool, counter))

_, _ = db.Customers.Query().With(Customers.Orders).All(ctx)
// counter saw 2 statements, whatever the row count
```

A tracer is the honest way to assert there is no N+1: the number is observed
rather than reasoned about.
