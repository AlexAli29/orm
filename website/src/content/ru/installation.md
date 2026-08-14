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
