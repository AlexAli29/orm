# Security policy

## Supported releases

Security fixes are made to the latest released minor of the current major.
Once v1.0.0 is released, that means the newest `v1.x`. Older minors are not
patched; upgrade within v1 is intended to be safe by the compatibility policy
in [docs/compatibility.md](docs/compatibility.md).

Pre-1.0 releases are not supported: report against the latest tag or `main`.

## Reporting a vulnerability

> **ACTION REQUIRED BEFORE v1.0.0.** The project owner must supply a private
> reporting channel and replace this block. Nothing here invents an address:
> publishing one that nobody reads is worse than publishing none, because it
> looks like a channel and silently is not.
>
> The recommended option for a GitHub-hosted project is **GitHub Private
> Vulnerability Reporting** (repository → Settings → Security → enable), which
> needs no email address and gives reporters a private thread and a CVE
> workflow. A dedicated address is the alternative.

Please **do not open a public issue** for a suspected vulnerability, and do not
disclose it publicly until a fix is available or a coordination window has
passed.

## What to include

- what the flaw allows, in one sentence;
- affected version or commit, PostgreSQL version, and Go version;
- a minimal reproduction — declarations, the query built, and the SQL produced;
- whether it needs untrusted input, and where that input enters.

## What this project treats as a vulnerability

- SQL injection through any documented API, including `orm.Raw` used as
  documented with bind arguments;
- an identifier or a bind value reaching emitted SQL unquoted or unescaped;
- credentials, bind values or the DSN appearing in a trace event, a log line, a
  span, or an error where the documentation says they will not;
- a generated migration that destroys data the declarations did not ask to
  remove;
- the health endpoints modifying the database.

## What it does not

- SQL you wrote yourself inside `orm.Raw` containing your own literals — the ORM
  cannot redact those without parsing SQL, and this is documented;
- `*pgconn.PgError` carrying PostgreSQL's own contextual detail, which the ORM
  passes through rather than rewriting;
- exposing a health, plan or diagnostic endpoint publicly without
  authentication. `/admin/db-health` is documented as needing it.

## Response

We aim to acknowledge within a week, agree an assessment and a fix window with
the reporter, and credit them unless they prefer otherwise.
