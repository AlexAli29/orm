---
title: Установка
description: Модуль, CLI и файл конфигурации.
---

## Модуль

```bash
go get github.com/AlexAli29/orm
```

Рантайм зависит только от стандартной библиотеки и `github.com/jackc/pgx/v5`. Эта граница проверяется тестом, поэтому она не размывается со временем.

## CLI

Генератор и планировщик миграций — один бинарник:

```bash
go install github.com/AlexAli29/orm/cmd/orm@latest
```

Или без установки — так это обычно и попадает в `Makefile`:

```bash
go run github.com/AlexAli29/orm/cmd/orm generate
```

Зафиксируйте версию в `tools.go`, если хотите отслеживать её вместе со всем остальным.

## Дополнительные адаптеры

Каждый — отдельный модуль, поэтому проект, который им не пользуется, его и не компилирует:

```bash
go get github.com/AlexAli29/orm/ormotel          # трейсинг OpenTelemetry
go get github.com/AlexAli29/orm/ormtest/postgres # помощники Testcontainers
```

`ormslog`, `ormhealth` и `ormtest` лежат в основном модуле и ничего не стоят, пока не импортированы.

## orm.yaml

Файл конфигурации лежит в корне проекта:

```yaml
version: 1

schema:
  # managed: схемой владеют декларации, миграции её применяют.
  # Уберите mode для режима database-first, где авторитетна база.
  mode: managed
  dsn: ${DATABASE_URL}
  search_path:
    - public

migrations:
  dir: migrations

packages:
  - path: ./internal/domain
    output: same

# Типы, которых в Go нет, приходят через конфигурацию. Библиотека отказывается
# выбирать за вас пакет для uuid: популярные варианты не взаимозаменяемы.
types:
  uuid:
    go: github.com/google/uuid.UUID
    codec: uuid
```

`${DATABASE_URL}` подставляется из окружения, поэтому файл не содержит секретов и его можно коммитить.

## Проверка установки

```bash
orm check
```

В пустом проекте это сообщит, что деклараций не найдено — правильный ответ, доказывающий, что CLI дотянулся до базы.

## Разобранные примеры

### Database-first, поверх существующей базы

Ни `mode`, ни каталога миграций: авторитетна база, а вы пишете декларации,
описывающие её:

```yaml
version: 1

schema:
  dsn: ${DATABASE_URL}
  search_path:
    - public
    - reporting

packages:
  - path: ./internal/domain
    output: same
```

### Managed с несколькими ограниченными контекстами

У каждого контекста свой пакет, и генератор пишет рядом с каждым:

```yaml
version: 1

schema:
  mode: managed
  dsn: ${DATABASE_URL}
  search_path:
    - public
    - billing
    - identity

migrations:
  dir: migrations

packages:
  - path: ./internal/billing/domain
    output: same
  - path: ./internal/identity/domain
    output: same
  - path: ./internal/catalog/domain
    output: same
```

Два контекста могут владеть таблицами с одинаковым именем в разных схемах: у них
будут разные дескрипторы и разное состояние миграций.

### Типы, которых в Go нет

```yaml
types:
  uuid:
    go: github.com/google/uuid.UUID
    codec: uuid
  numeric:
    go: github.com/shopspring/decimal.Decimal
    codec: decimal
```

Это те два, которые библиотека отказывается выбирать за вас: популярные пакеты
не взаимозаменяемы, а неверный `numeric` тихо портит деньги.

### Makefile, который держит всё в согласии

```makefile
generate:
	go run github.com/AlexAli29/orm/cmd/orm makemigrations
	go run github.com/AlexAli29/orm/cmd/orm migrate
	go run github.com/AlexAli29/orm/cmd/orm generate

check:
	go run github.com/AlexAli29/orm/cmd/orm makemigrations --check
	go run github.com/AlexAli29/orm/cmd/orm check --generated
```
