---
title: PostGIS
description: Пространственные типы, которые остаются пространственными: geometry и geography порознь.
---

## Подключается отдельно

Поддержка PostGIS — отдельный пакет. Проект, который его не импортирует, никогда
не увидит пространственного API, а корневая библиотека ничего не знает о
геометрии:

```go
import "github.com/AlexAli29/orm/postgis"
```

Всё в нём собирается через ту единственную границу расширения, которую открывает
корневой пакет. Нет ни второго компилятора запросов, ни второй модели выражений:
пространственный предикат — это такой же `orm.Predicate`, и он вкладывается в
составные запросы, CTE и производные таблицы без изменений.

## Два различения, которые никогда не смешиваются

**geometry** — декартова, в тех единицах, которые задаёт система координат SRID.
**geography** — на сфероиде, расстояния и длины в метрах.

Это разные типы PostgreSQL с разным поведением индексов и разными ответами,
поэтому здесь это разные типы Go. Преобразование между ними — то, что вы пишете
сами, а не то, что случается с вами.

И два факта путешествуют вместе с каждым значением и каждой колонкой:

- **форма** — Point, LineString, Polygon и их множественные варианты;
- **SRID** — в какой системе координат эти числа.

Потеря любого из них — это способ получить запрос, сравнивающий метры с
градусами и возвращающий число.

## Объявление пространственной колонки

Тег `pgtype` несёт форму и систему координат, потому что из Go-типа не выводится
ни то, ни другое:

```go
//orm:table public.places
type Place struct {
    ID   int64  `orm:"pk,identity"`
    Name string

    // На сфероиде. Расстояния приходят в метрах.
    Spot postgis.Geography `orm:"pgtype:geography(Point,4326)"`

    // Декартова, в градусах WGS 84.
    Location postgis.Geometry `orm:"pgtype:geometry(Point,4326)"`

    // То же место в web Mercator. Соотнести его с Location без преобразования —
    // ошибка, которую SRID делает видимой.
    Projected *postgis.Geometry `orm:"pgtype:geometry(Point,3857)"`

    Footprint *postgis.Geometry `orm:"pgtype:geometry(Polygon,4326)"`
}
```

Указатель — nullable-колонка, как и везде. Генератор выдаёт `GeomCol`, `GeogCol`
и их nullable-формы, каждая несёт объявленные SRID, вид и размерность.

## Запросы

`postgis.Of` поднимает колонку geometry в пространственное выражение,
`postgis.OfGeog` — то же для geography:

```go
// Всё в пределах 5 км от точки, на сфероиде — в метрах, потому что geography
// измеряет в метрах.
here := postgis.GeographyPoint(-0.1276, 51.5072)

places, err := db.Places.Query().
    Where(postgis.OfGeog(Places.Spot).
        DWithin(postgis.GeogValue[Place](here), 5000)).
    All(ctx)
```

```go
// Декартовы отношения, на geometry.
db.Places.Query().Where(postgis.Of(Places.Location).Intersects(v))
db.Places.Query().Where(postgis.Of(Places.Location).Within(v))
db.Places.Query().Where(postgis.Of(Places.Location).Contains(v))
```

### Операторы по ограничивающей рамке названы именно так

```go
postgis.Of(Places.Location).BBoxIntersects(v)  // &&
postgis.Of(Places.Location).BBoxContains(v)    // ~
postgis.Of(Places.Location).BBoxWithin(v)      // @
```

`&&` — это не `ST_Intersects`. Он сравнивает ограничивающие рамки: это дешевле и
отвечает на другой вопрос, поэтому у него другое имя, а не вид «быстрой версии»
точного оператора.

## Измерения и преобразования

Они возвращают обычный `orm.Value`, поэтому годятся в проекции и сортировки как
всё остальное:

```go
distance := postgis.OfGeog(Places.Spot).Distance(postgis.GeogValue[Place](here))

type Near struct {
    Name   string
    Metres float64
}

var near = orm.Project2(
    Places.Name, distance,
    func(name string, m float64) Near { return Near{name, m} },
)

rows, err := orm.Select(db.Places, near).
    OrderBy(distance.Asc()).
    Limit(20).
    All(ctx)
```

На выражении доступны также `Area`, `Length`, `Centroid`, `Buffer`, `Boundary`,
`Azimuth`, `AsText`, `AsEWKT`, `AsGeoJSON`, `AsBinary`, `AsEWKB` и
`AsGeography` — то самое преобразование, которое вы делаете осознанно. У каждого
есть форма `…Null` для nullable-колонки, потому что измерение NULL-геометрии —
это NULL.

## Агрегаты

```go
postgis.Collect(g)   // ST_Collect   -> *Geometry
postgis.UnionAgg(g)  // ST_Union     -> *Geometry
postgis.Extent(g)    // ST_Extent    -> *Box2D
postgis.Extent3D(g)  // ST_3DExtent  -> *Box3D
```

## Регистрация типов

pgx нужно рассказать о типах PostGIS на каждом соединении:

```go
cfg.AfterConnect = func(ctx context.Context, conn *pgx.Conn) error {
    return postgis.Register(ctx, conn)
}
```

`RegisterIfPresent` — терпимая форма: она сообщает, было ли расширение, вместо
того чтобы падать. Это то, что нужно бинарнику, который работает и с
пространственными базами, и с обычными.

## На каких версиях это доказано

PostgreSQL 17 с PostGIS 3.5, 16 с 3.4 и 14 с 3.4.

Пространственный набор тестов пропускается, когда расширения нет, — правильно на
машине разработчика и неправильно в CI, поэтому CI ставит
`ORM_REQUIRE_POSTGIS=1`, что превращает пропуск в падение. Заявленной поддержке,
которую ничто не проверяет, верить нельзя.

Библиотека никогда не создаёт расширение. `CREATE EXTENSION postgis` —
привилегированная операция того, кто владеет базой.
