---
title: JSON и JSONB
description: Чтение внутрь документа и почему любое чтение возвращает nullable.
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
db.Users.Update(ctx).
    SetExpr(Users.Settings,
        orm.JSONSet(Users.Settings, []string{"plan"}, newPlan, true)).
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
