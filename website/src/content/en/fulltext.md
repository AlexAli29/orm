---
title: Full-text search
description: tsvector, tsquery and ranking — the pieces PostgreSQL has, kept apart.
---

## The two types

`tsvector` is a document, processed into lexemes. `tsquery` is a search
expression. They are different types and matching is an operator between them,
which is why this is not a single `Search(string)` method: the vector usually
lives in a column, and the query is built per request.

```go
//orm:table public.articles
type Article struct {
    ID     int64       `orm:"pk,identity"`
    Title  string
    Body   string
    Search orm.TSVector `orm:"pgtype:tsvector"`
}
```

## Matching

```go
q := orm.PlainToTSQuery(orm.English, userInput)

articles, err := db.Articles.Query().
    Where(orm.Matches(Articles.Search, q)).
    All(ctx)
```

```sql
search @@ plainto_tsquery('english', $1)
```

`orm.Matches` is a free function taking the vector and the query, because
either side can be a column or an expression.

## Building a query

Four constructors, and the difference between them is how they treat the user's
text:

```go
orm.PlainToTSQuery(orm.English, "postgres indexing")
// every word ANDed; punctuation ignored. The safe default for a search box.

orm.PhraseToTSQuery(orm.English, "index only scan")
// the words in that order, adjacent

orm.WebSearchToTSQuery(orm.English, `"index only" -bitmap`)
// Google-ish syntax: quoted phrases, OR, and leading minus for NOT

orm.ToTSQuery(orm.English, "index & postgres")
// raw tsquery syntax — & | ! <-> — and it errors on malformed input
```

Only the last takes operator syntax, so only the last can fail on what a user
typed. That is the one to keep away from a public search box.

Combining:

```go
orm.AndTSQuery(a, b)
orm.OrTSQuery(a, b)
orm.NotTSQuery(a)
```

## Configurations

The first argument is the text-search configuration, which decides stemming and
stop words:

```go
orm.English   // "english"
orm.Simple    // "simple" — no stemming, no stop words
orm.TextSearchConfig("russian")
```

It is a named string type, so any configuration the server has is available
without waiting for a constant to be added.

## Ranking

```go
q := orm.PlainToTSQuery(orm.English, input)
rank := orm.TSRank(Articles.Search, q)

type Hit struct {
    Title string
    Rank  float32
}

var hits = orm.Project2(
    Articles.Title, rank,
    func(title string, r float32) Hit { return Hit{title, r} },
)

rows, err := orm.Select(db.Articles, hits).
    Where(orm.Matches(Articles.Search, q)).
    OrderBy(rank.Desc()).
    Limit(20).
    All(ctx)
```

`TSRankCD` is cover-density ranking, which accounts for how close the matched
lexemes are to each other. Both have `…Null` forms for a nullable vector.

Note that the query is built **once** and used twice — in the `WHERE` and in the
ranking. Building it twice would put the same text in two bind parameters and
make the planner work harder for no reason.

## Building a vector in SQL

When the column is text rather than a stored `tsvector`:

```go
vec := orm.ToTSVector(orm.English, Articles.Body)
db.Articles.Query().Where(orm.Matches(vec, q))
```

That cannot use a `tsvector` index, so it is for occasional queries rather than
for the search path. For the search path, store the vector in a column — usually
a generated one — and index it.

## Weights

```go
title := orm.SetWeight(orm.ToTSVector(orm.English, Articles.Title), orm.WeightA)
body  := orm.SetWeight(orm.ToTSVector(orm.English, Articles.Body), orm.WeightB)
both  := orm.Concat2TSVector(title, body)
```

`WeightA` through `WeightD` are what make a title match outrank a body match.
Ranking reads them; matching ignores them.
