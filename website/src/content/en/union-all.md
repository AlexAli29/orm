---
title: UNION ALL
description: Composing two typed SELECTs into one, with duplicates kept.
---

## Scope

v1 composes `UNION ALL` and nothing else. `UNION`, `INTERSECT` and `EXCEPT` are not part of it, and the compiler refuses any other set operation rather than leaving a gap somebody discovers at run time.

`UNION ALL` keeps duplicate rows. That is the operation: if you wanted them removed you wanted a different one.

## The rules

Branches must have **exactly** compatible output shapes — same column count, same order, same Go result types, same nullability. The v1 contract is deliberately stricter than PostgreSQL's implicit coercion:

- `int32` and `int64` are not merged.
- `uuid.UUID` and `string` are not merged.
- A non-nullable and a nullable column are not merged.

PostgreSQL could find a common type for some of those. Letting it would make the Go result type depend on a coercion rule nobody reads, so the answer is a refusal with the mismatch named.

## Branch-local clauses

A branch may carry its own `ORDER BY`, `LIMIT` and `OFFSET`, and it means what it says:

```sql
(SELECT ... ORDER BY placed DESC LIMIT 2)
UNION ALL
(SELECT ... ORDER BY placed DESC LIMIT 2)
LIMIT 3
```

The parentheses are the grammar, not a style. Written bare, PostgreSQL attaches those clauses to the whole compound — so a branch that looks limited is not, and the rows you get are not the rows you asked for. The compiler parenthesises a branch exactly when it carries one of them.

## Compound clauses

`ORDER BY`, `LIMIT` and `OFFSET` on the compound apply to the complete result, after both branches:

```sql
SELECT ... UNION ALL SELECT ... ORDER BY "email" ASC LIMIT 10
```

A compound's `ORDER BY` may name an **output column** and nothing else. That is PostgreSQL's rule: a qualified reference gets `missing FROM-clause entry`, and an expression gets `invalid UNION/INTERSECT/EXCEPT ORDER BY clause`. Without an outer `ORDER BY`, nothing about the order of the result is promised.

## Placeholders are global

The branches share one parameter list:

```sql
SELECT ... WHERE email = $1
UNION ALL
SELECT ... WHERE label = $2
```

Restarting numbering in the second branch would produce SQL PostgreSQL accepts and binds the wrong values into — which is why this is the property the implementation leads with.

## Scope is per branch

A branch sees the compound's `WITH` items and its own sources. It does not see the other branch's:

```go
// refused: the right branch names a source only the left introduces
```

A branch is not a scope-sharing mechanism, and that is structural — each branch pushes its own scope frame.

## Nesting

`A UNION ALL B UNION ALL C` is one operation over three inputs, so it renders flat. Building it as `A UNION ALL (B UNION ALL C)` parenthesises the inner one, because at that point you asked for it — and because the inner compound's own `ORDER BY` and `LIMIT` would otherwise bind to the outer.

## One statement

A compound is one SQL statement. It is never two queries whose rows are appended in Go — that would lose the compound `ORDER BY`, lose `LIMIT`, and turn one round trip into two.
