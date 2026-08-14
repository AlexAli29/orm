# Support policy

What v1 runs on, why those versions, and where each claim is proven.

A version is listed here only if CI runs the suite against it. A README that
claims a version nothing tests is a claim nobody should believe.

## PostgreSQL

**Policy: every major PostgreSQL that upstream still supports.**

The PostgreSQL Global Development Group supports a major for five years. This
project follows that window rather than inventing its own, because the useful
question for an operator is "does it work on a server I am allowed to be
running", and upstream already answers it.

| Major | Upstream support ends | Tested in CI |
|---|---|---|
| 14 | 12 November 2026 | yes |
| 15 | November 2027 | yes |
| 16 | November 2028 | yes |
| 17 | November 2029 | yes |
| 18 | November 2030 | yes |

**PostgreSQL 14 is supported until its upstream end of life on 12 November
2026, and dropped in the first minor release after that.** That is a deliberate
decision recorded here rather than a silent drift: it is not removed early, and
it is not carried forever by accident. Dropping it is documented in the
changelog as a supported-platform change, not as a breaking API change — the API
does not change, the tested matrix does.

Features that exist only on newer majors are gated at runtime from
`server_version_num` and are refused with a typed error on servers that lack
them, never silently downgraded. Multirange, the newer `EXPLAIN` options and
the catalog columns introspection reads are each exercised on every major above.

## PostGIS

PostGIS is optional. Without it, the `postgis` package's types and operators are
unusable and everything else works; with a version older than supported, the
codec fails with `*postgis.NotInstalledError` or a version error rather than
producing wrong geometry.

| Combination | Tested |
|---|---|
| PostgreSQL 17 + PostGIS 3.5 | yes |
| PostgreSQL 16 + PostGIS 3.4 | yes |
| PostgreSQL 14 + PostGIS 3.4 | yes |

Only combinations PostGIS upstream ships and CI runs are claimed. Others may
work; they are not promised.

## Go

**Minimum: Go 1.24. Tested: Go 1.24 and current stable.**

The `go` directive in `go.mod` is a real minimum — a toolchain older than it
refuses to build the module — so it is set to the oldest version the code
actually needs, not to whatever the maintainer happens to have installed. Go
upstream supports only the latest two majors, and this floor is deliberately
below that: a library that demands the newest toolchain excludes users for the
maintainer's convenience.

CI builds and tests on the floor with `GOTOOLCHAIN=local`, so an accidental use
of a newer standard-library function or language feature fails there rather than
reaching a user. It also tests current stable, so the project does not rot.

The floor rises only in a minor release, only when a language feature or a
dependency requires it, and always with a changelog entry.

### Modules with a higher floor

`ormtest/postgres` requires **Go 1.25**, because Testcontainers depends on
grpc-gateway, which requires it. `examples/production` and `examples/hexagonal`
require it too, for the same kind of reason: their dependencies moved, and an
example is where a real dependency tree is allowed to be.

None of that reaches the library. The ORM itself, `ormotel` and the extension SDK
build on 1.24, and CI proves it with `GOTOOLCHAIN=local` in the `minimum-go` job,
which touches those three modules and nothing else.

What it does reach is the jobs that *handle* those modules. A tool built with an
older Go cannot type-check a module that declares a newer one — the limit is the
tool's own build version, not the toolchain in `PATH`, so `GOTOOLCHAIN` does not
help. The jobs that build the examples, or point `orm` and `apimanifest` at
them, therefore run on 1.25. That is a fact about the tooling rather than a claim
about the floor, and the two are kept in separate jobs so neither can be mistaken
for the other.

### Running `go mod tidy`

Run it with the floor toolchain:

```sh
GOTOOLCHAIN=go1.24.0 go mod tidy
```

`go mod tidy` under a newer toolchain speculatively resolves test dependencies
of dependencies, and one of them (`github.com/rogpeppe/go-internal`) declares
Go 1.25. Tidying with a 1.26 toolchain therefore rewrites the `go` directive to
1.25 and adds two indirect requirements that **nothing in this module imports** —
`go list -deps -test ./...` lists neither. The effect is a floor raised for no
reason, which is the opposite of the policy above.

On the floor toolchain, tidy leaves every module unchanged.
