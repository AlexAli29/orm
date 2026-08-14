---
title: Compatibility
description: Which PostgreSQL versions, which Go versions, and what stability means here.
---

## PostgreSQL

**14, 15, 16, 17 and 18.**

Not "should work on". The compatibility suite refuses to run unless all five are reachable at once:

```text
the compatibility matrix requires every supported major and
[ORM_TEST_DSN_PG14 ... PG18] are unset. Skipping here would report a five-major
claim proven by however many servers happened to be running
```

A version listed as supported and never run against is a claim nobody should believe.

14 leaves the list when upstream ends its support on 12 November 2026.

## What is proved across all five

- The whole user workflow: migrate, generate, check, write, read, join, refresh.
- **Byte-identical artifacts.** Generated Go, `orm.lock` and migration artifacts are the same on 14 and on 18. A mixed-server team gets no diff on checkout.
- No server-local content in any artifact: no OIDs, no database name, no server version, no deparsed definition, no absolute paths.

## Go

The floor is the version in `go.mod`, currently **1.24**, and raising it is a decision rather than a side effect of a newer toolchain being available. A dedicated CI job pins `GOTOOLCHAIN=local` and builds only the modules that are on the floor, so the claim is proved rather than assumed.

Some peripheral modules declare a higher version because their own dependencies do — the Testcontainers helper and some examples need 1.25. That does not move the library's floor, and the jobs are separated so it cannot.

## PostGIS

Proved on the combinations the project actually claims: PostgreSQL 17 with PostGIS 3.5, 16 with 3.4, and 14 with 3.4. The spatial suite skips when the extension is unavailable — right on a developer's machine, wrong in CI — so CI sets `ORM_REQUIRE_POSTGIS=1`, which turns the skip into a failure.

## Stability

The public API is frozen at v1 and tracked by a generated manifest. A removed symbol, a changed signature, a tightened constraint or a method added to an interface consumers implement all fail the build. The manifest tool is itself tested for noticing each of those.

## Extensions

`citext`, `hstore`, `pg_trgm`, `uuid-ossp` and PostGIS are recognised when present. None is required, and the ORM never creates an extension — that is a privileged operation belonging to whoever owns the database.
