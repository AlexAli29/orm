# Writing a PostgreSQL extension for this ORM

PostGIS proved the model; this is the model written down. An extension package
adds PostgreSQL functionality the core does not, using only the exported API —
no access to the AST, the writer, the scope checker, or anything under
`internal/`.

`ormextdemo/` is a worked example that runs the conformance suite below.

## The boundary

Four public pieces are the whole of it:

```go
orm.Arg                       // an operand, opaque by design
orm.ArgOf / orm.ArgOpt        // a typed expression, or its nullable form
orm.ArgValue / orm.ArgCast    // a Go value — becomes a bind parameter
orm.FnExpr / orm.OpPredicate  // a call or an operator over those operands
```

An extension names a function and describes a shape:

```go
func MD5[E any](v orm.Selectable[E, string]) orm.Expression[string, *string] {
    return orm.FnExpr[string]("md5", orm.ArgOf(v))
}
```

The name is syntax and comes from the extension's own code. Every **value** goes
through `ArgValue` and becomes a bind parameter — there is no way to splice one
into the statement text, which is what makes the SDK safe to hand to a third
party.

What you get for free, without implementing any of it: entity tagging, source
identity, scope validation, outer-join nullability, the placeholder namespace,
and `*pgconn.PgError` preservation.

## The trust boundary

Three different things, and confusing them is how an extension ships a bug:

| | Who guarantees it |
| --- | --- |
| Scope, source identity, placeholder numbering, nullability propagation | **The core enforces it.** An extension cannot get these wrong. |
| The result type and nullability an extension declares | **The extension claims it.** Nothing verifies it. |
| That the SQL means what the extension thinks | **The server decides.** |

`FnExpr[string]("md5", …)` says "PostgreSQL returns text here" and the ORM
believes it. If the function actually returns an integer the mistake surfaces as
a **scan error, not a wrong value** — but it is still the extension's mistake,
and there is a conformance test for exactly this.

Do not read this as "third-party extension code cannot be wrong". It can. The
conformance suite is how an author checks their own side.

## Keeping out of `internal/`

Go's internal rule works on **import paths, not modules**. A module named
`github.com/AlexAli29/orm/yourext` sits lexically inside the ORM, and the
compiler will happily let it import `internal/expr` — a separate `go.mod` does
not help.

Name your module as what it is:

```
module github.com/you/yourext      // the compiler now refuses internal imports
```

`ormextdemo` uses `example.com/ormextdemo` for this reason, with a test pinning
it. Keep a static import check in your own suite too; it costs nothing and it
catches the day somebody adds a convenient shortcut.

## Conformance suite

Run these against a real server. They are the questions this project's own
audits asked of PostGIS:

| Check | What it catches |
| --- | --- |
| `pg_typeof` differential | A declared Go type that does not match what PostgreSQL returns |
| NULL semantics | A non-nullable declaration over a function that returns NULL |
| Outer-join nullability | An expression that stays non-null through a `LEFT JOIN` |
| Source dependencies | An operand whose source vanished — the forging case |
| Placeholder numbering | A nested call whose bind parameter took the wrong number |
| Wrong result type | Confirms the failure is a scan error, not a wrong value |
| Race | Descriptors and the SDK are read-only |
| Static imports | Nothing internal is reachable |

Add, where your extension has them: COPY round-trip, database-first mapping,
managed-schema mapping, migration round-trip, generation determinism.

## Playbook: adding a custom PostgreSQL type

`ormextdemo` covers expressions. A type is more work, in this order — each step
is checkable before the next:

1. Define the Go value type. Decide what SQL `NULL` is: the ORM's convention is
   `T` for `NOT NULL` and `*T` for nullable, with no `Valid` flag inside the
   value.
2. Implement or register the pgx codec. Prefer pgx's own protocol interfaces
   over writing a text parser — see how `orm.Range[T]` implements
   `pgtype.RangeValuer`.
3. Decide the canonical PostgreSQL type identity. Two Go-identical types can be
   two PostgreSQL types (`daterange` vs `tstzrange`), and the catalog is the
   authority.
4. Add reconciliation, so database-first checks the mapping.
5. Add the generated descriptor and its capabilities.
6. Add the expressions and operators. Watch operator overload resolution — a
   bare bind parameter under an overloaded operator gets the wrong overload, and
   the fix is casting the operand to the catalog type.
7. Add writes, projections and COPY.
8. Verify `pg_typeof` for every expression.
9. Verify NULL semantics for every one.
10. Verify source dependencies and outer-join nullability.
11. Verify the database-first workflow.
12. Verify managed schema and migrations, including that a changed type is
    visible as drift.
13. Fuzz the value model, comparing against the server's canonical form rather
    than against what you sent.
14. Run `-race`.

## Versioning

An extension package defines its own compatibility policy: which PostgreSQL
versions, which extension versions, which ORM versions. The core makes no claim
that every combination works, and neither should you — say what you test.
