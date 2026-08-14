---
title: Полнотекстовый поиск
description: tsvector, tsquery и ранжирование — части, которые есть в PostgreSQL, оставленные порознь.
---

## Два типа

`tsvector` — это документ, разобранный на лексемы. `tsquery` — поисковое
выражение. Это разные типы, а совпадение — оператор между ними; поэтому здесь нет
одного метода `Search(string)`: вектор обычно лежит в колонке, а запрос строится
на каждый вызов.

```go
//orm:table public.articles
type Article struct {
    ID     int64       `orm:"pk,identity"`
    Title  string
    Body   string
    Search orm.TSVector `orm:"pgtype:tsvector"`
}
```

## Совпадение

```go
q := orm.PlainToTSQuery(orm.English, userInput)

articles, err := db.Articles.Query().
    Where(orm.Matches(Articles.Search, q)).
    All(ctx)
```

```sql
search @@ plainto_tsquery('english', $1)
```

`orm.Matches` — свободная функция, принимающая вектор и запрос, потому что с
любой стороны может быть колонка или выражение.

## Построение запроса

Четыре конструктора, и различаются они тем, как обходятся с текстом
пользователя:

```go
orm.PlainToTSQuery(orm.English, "postgres indexing")
// все слова через AND, пунктуация игнорируется. Безопасное умолчание для строки поиска.

orm.PhraseToTSQuery(orm.English, "index only scan")
// слова в этом порядке, подряд

orm.WebSearchToTSQuery(orm.English, `"index only" -bitmap`)
// синтаксис в духе Google: кавычки, OR и минус впереди как NOT

orm.ToTSQuery(orm.English, "index & postgres")
// сырой синтаксис tsquery — & | ! <-> — и он падает на некорректном вводе
```

Только последний принимает синтаксис операторов, поэтому только он может упасть
на том, что ввёл пользователь. Именно его не стоит подключать к публичной строке
поиска.

Комбинирование:

```go
orm.AndTSQuery(a, b)
orm.OrTSQuery(a, b)
orm.NotTSQuery(a)
```

## Конфигурации

Первый аргумент — конфигурация полнотекстового поиска, она определяет стемминг и
стоп-слова:

```go
orm.English   // "english"
orm.Simple    // "simple" — без стемминга и стоп-слов
orm.TextSearchConfig("russian")
```

Это именованный строковый тип, поэтому доступна любая конфигурация, которая есть
на сервере, — не дожидаясь появления константы.

## Ранжирование

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

`TSRankCD` — ранжирование по плотности покрытия: оно учитывает, насколько близко
совпавшие лексемы друг к другу. У обоих есть формы `…Null` для nullable-вектора.

Обратите внимание: запрос строится **один раз** и используется дважды — в `WHERE`
и в ранжировании. Построить его дважды значило бы положить один и тот же текст в
два параметра и заставить планировщик работать больше без причины.

## Построение вектора в SQL

Когда в колонке текст, а не хранимый `tsvector`:

```go
vec := orm.ToTSVector(orm.English, Articles.Body)
db.Articles.Query().Where(orm.Matches(vec, q))
```

Так нельзя воспользоваться индексом по `tsvector`, поэтому это для разовых
запросов, а не для основного поиска. Для основного — храните вектор в колонке,
обычно генерируемой, и индексируйте её.

## Веса

```go
title := orm.SetWeight(orm.ToTSVector(orm.English, Articles.Title), orm.WeightA)
body  := orm.SetWeight(orm.ToTSVector(orm.English, Articles.Body), orm.WeightB)
both  := orm.Concat2TSVector(title, body)
```

`WeightA`…`WeightD` — то, из-за чего совпадение в заголовке весит больше, чем в
теле. Ранжирование их читает, поиск совпадений — игнорирует.

## Разобранные примеры

### База знаний

Ранжированные результаты, запрос построен один раз:

```go
q := orm.PlainToTSQuery(orm.English, input)
rank := orm.TSRank(Articles.Search, q)

var hits = orm.Project3(
    Articles.Slug, Articles.Title, rank,
    func(slug, title string, r float32) Hit { return Hit{slug, title, r} },
)

rows, err := orm.Select(db.Articles, hits).
    Where(orm.Matches(Articles.Search, q)).
    OrderBy(rank.Desc(), Articles.Title.Asc()).
    Limit(20).
    All(ctx)
```

Дополнительная сортировка по заголовку важна: без неё две статьи с одинаковым
рангом приходят в том порядке, который выдал план, а он меняется между
запусками.

### Строка поиска, принимающая операторы

```go
// Пользователь может ввести: "index only" -bitmap
q := orm.WebSearchToTSQuery(orm.English, input)
```

`WebSearchToTSQuery` понимает кавычки, `OR` и минус впереди и не падает на
некорректном вводе. `ToTSQuery` принимает сырой синтаксис `& | ! <->` и падает на
лишнем операторе, поэтому ему место за админской формой, а не перед публикой.

### Заголовок весит больше тела

```go
title := orm.SetWeight(orm.ToTSVector(orm.English, Recipes.Title), orm.WeightA)
body  := orm.SetWeight(orm.ToTSVector(orm.English, Recipes.Method), orm.WeightB)
doc   := orm.Concat2TSVector(title, body)

orm.Compose(pool, shape).From(Recipes.Source()).
    Where(orm.Matches(doc, q)).
    OrderBy(orm.TSRank(doc, q).Desc()).
    All(ctx)
```

Вычисленный так, он не может воспользоваться индексом, поэтому подходит для
админского отчёта. Для основного поиска храните взвешенный вектор в колонке и
индексируйте её.

### Фильтрация и поиск вместе

```go
orm.Select(db.Articles, hits).
    Where(Articles.Locale.Eq("en")).
    Where(Articles.Published.Eq(true)).
    Where(orm.Matches(Articles.Search, q)).
    OrderBy(rank.Desc()).
    All(ctx)
```
