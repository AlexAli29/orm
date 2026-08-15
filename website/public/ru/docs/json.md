# JSON и JSONB

> Чтение внутрь документа и почему любое чтение возвращает nullable.

Source: https://ormgo.vercel.app/ru/docs/json/
Symbols: https://ormgo.vercel.app/api/orm.txt — the generated list of every exported name.

---
## Колонка

Колонка `jsonb` отображается в тот Go-тип, который вы для неё объявили: обычно
это карта, иногда структура:

```go
//orm:table public.users
type User struct {
    ID       int64          `orm:"pk,identity"`
    Settings map[string]any `orm:"pgtype:jsonb"`
    Profile  *Profile       `orm:"pgtype:jsonb"`
}
```

Использовать стоит `jsonb`. `json` хранит исходный текст вместе с пробелами и
дублирующимися ключами; `jsonb` хранит разобранную структуру, а именно она нужна
индексам и операторам вхождения.

## Всё это свободные функции

Чтения и проверки — свободные функции, а не методы, потому что с любой стороны
может быть колонка или выражение. Они принимают `Optional`, поэтому не-nullable
колонка поднимается через `orm.Opt`:

```go
meta := orm.Opt(Users.Settings)
```

Они дают `Predicate[Composed]` или `Expression`, поэтому их место в составном
запросе.

## Проверки

```go
orm.JSONHasKey(meta, "plan")                    // ?
orm.JSONHasAnyKeys(meta, "plan", "tier")        // ?|
orm.JSONHasAllKeys(meta, "plan", "tier")        // ?&
orm.JSONContains(meta, orm.Val(v))              // @>
orm.JSONContainedBy(meta, orm.Val(v))           // <@
orm.JSONPathExists(meta, "$.billing.tier")      // @?
orm.JSONMatches(meta, "$.age > 18")             // @@
```

`JSONPathExists` и `JSONMatches` принимают синтаксис SQL/JSON path — самый
выразительный: `$.items[*].price`, фильтры, шаблоны.

## Чтение

```go
orm.JSONGet(meta, "billing")            // ->   ключ,    возвращает jsonb
orm.JSONText(meta, "plan")              // ->>  ключ,    возвращает text
orm.JSONIndex(meta, 0)                  // ->   индекс массива
orm.JSONIndexText(meta, 0)              // ->>  индекс
orm.JSONPathGet(meta, "billing", "tier")  // #>  путь, возвращает jsonb
orm.JSONPathText(meta, "billing", "tier") // #>> путь, возвращает text
orm.JSONArrayLength(meta)               // jsonb_array_length
orm.JSONTypeOf(meta)                    // jsonb_typeof
```

**Все они nullable, и это не осторожность.** `->` по отсутствующему ключу — это
NULL. `->>` по несуществующему пути — NULL. `jsonb_typeof` от NULL-документа —
NULL. Документ — это форма, которую никто не проверял, поэтому чтение, обещающее
не-NULL, врало бы про самый частый случай.

Поэтому вы приводите тип, а не сравниваете напрямую:

```go
age := orm.CastNull(orm.JSONPathText(meta, "profile", "age"), orm.Integer)
// Expression[*int32, *int32]
```

## Запись

```go
orm.JSONSet(Users.Settings, []string{"billing", "tier"}, v, true)
orm.JSONInsert(Users.Settings, []string{"tags", "0"}, v, false)
orm.JSONStripNulls(Users.Settings)
```

Последний аргумент `JSONSet` — это `create_missing`: добавлять ли ключ, если пути
нет. У `JSONInsert` это `insert_after`. Оба — булевы в собственной сигнатуре
PostgreSQL, и они переданы как есть, а не переименованы: читатель, заглянувший в
руководство, должен найти тот же аргумент.

Они возвращают `Value`, поэтому их место в обновлении:

```go
db.Users.Update().
    Set(Users.Settings.SetExpr(orm.JSONSet(Users.Settings, []string{"plan"}, newPlan, true))).
    Where(Users.ID.Eq(id)).
    Exec(ctx)
```

## Индексы

Запросу на вхождение нужен GIN-индекс:

```go
//orm:index users_settings_gin_idx (Settings) using gin
```

`jsonb_path_ops` меньше и быстрее для одного лишь `@>`, но поддерживает меньше
операторов — объявляйте его как индекс по выражению, если он нужен.

## Разобранные примеры

### Флаги функций у аккаунта

```go
settings := orm.Opt(Accounts.Settings)

// Аккаунты, включившие бету.
orm.Compose(pool, shape).From(Accounts.Source()).
    Where(orm.JSONContains(settings, orm.Val(map[string]any{"beta": true})))

// Аккаунты, где ключ вообще не задавали, — это другой вопрос.
orm.Compose(pool, shape).From(Accounts.Source()).
    Where(orm.Not(orm.JSONHasKey(settings, "beta")))
```

`Contains` спрашивает про значение, `HasKey` — принимал ли кто-то решение. Флаг,
которого нет, и флаг, равный `false`, — разные состояния, и так их различают.

### Полезная нагрузка события

Чтение вложенного значения и сравнение его как числа:

```go
payload := orm.Opt(Events.Payload)

amount := orm.CastNull(orm.JSONPathText(payload, "order", "total"), orm.Integer)

var big = orm.Project2(
    orm.Of(Events.ID), amount,
    func(id int64, total *int32) Big { return Big{id, total} },
)

orm.Compose(pool, big).From(Events.Source()).
    Where(orm.JSONPathExists(payload, "$.order.total")).
    All(ctx)
```

Тип определяется приведением. `->>` возвращает текст, что бы ни лежало в
документе, и сравнение с числом обязано это сказать.

### Документ профиля, правка на месте

```go
db.Profiles.Update().
    Set(Profiles.Doc.SetExpr(orm.JSONSet(Profiles.Doc, []string{"contact", "email"}, newEmail, true))).
    Where(Profiles.ID.Eq(id)).
    Exec(ctx)
```

`true` — это `create_missing`: добавить `contact.email`, если пути нет. С `false`
обновление ничего не сделает с документом, где его никогда не было.

### Вопросы о форме

```go
orm.JSONTypeOf(orm.Opt(Events.Payload))       // "object", "array", "string"…
orm.JSONArrayLength(orm.Opt(Events.Items))    // *int32, NULL, если это не массив
```
