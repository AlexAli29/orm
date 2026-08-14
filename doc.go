// Package orm is the runtime of a schema-reconciling PostgreSQL data mapper.
//
// The thesis is a division of ownership:
//
//	You own your structs. PostgreSQL owns your schema. The generator proves they agree.
//
// Neither representation is generated from the other. Entity structs are written
// by hand and marked with an //orm:table directive; the schema is owned by
// migrations. The orm command introspects both, reports every place they
// disagree, and generates typed metadata only from a mapping it proved.
//
// This package is what generated code is written against and what applications
// call. The generator, the reconciler and the command line tool live under gen/
// and cmd/orm, and are never imported by runtime code.
//
// # Reading
//
// A [Repo] binds generated metadata to an [Executor] — a *pgxpool.Pool, a
// *pgx.Conn or a pgx.Tx. [Repo.Query] returns a [Query], whose conditions are
// [Predicate] values built from generated column descriptors:
//
//	users, err := db.Users.Query().
//	    Where(Users.Email.ILike("%@example.com")).
//	    OrderBy(Users.CreatedAt.Desc()).
//	    Limit(50).
//	    All(ctx)
//
// The type parameters are the point. A Predicate[User] cannot reach a query over
// Post, a text column has Like and an integer does not, and a NOT NULL column has
// no IsNull — all decided by the compiler, from what reconciliation proved about
// the real schema.
//
// A Query is mutable and single-use; [Query.Clone] branches one. Builder
// mistakes accumulate and surface together from the terminal operation, so a
// query that cannot be built never reaches PostgreSQL. Terminals are
// [Query.All], [Query.One], [Query.Count], [Query.Exists] and [Query.Rows];
// [Query.SQL] renders the statement without running it.
//
// # Writing
//
// [Repo.Insert], [Repo.InsertMany], [Repo.Update] and [Repo.Delete] state
// intent. Nothing infers it: there is no Save, no dirty tracking and no identity
// map.
//
// A Go zero value is a value. A struct with Active set to false stores false,
// because this package cannot tell that from a field somebody left alone —
// and guessing is how a row ends up with a value nobody chose. Asking for the
// column's default is [Default], which is a separate and explicit thing.
//
// An update or delete with no WHERE clause is refused with [ErrMissingWhere]
// unless All says every row was meant.
//
// # Relations
//
// [Many] and [One] declare relations on an entity and record three states:
// unloaded, loaded and empty, loaded and present. The zero value is unloaded, so
// a struct literal that omits a relation says "I did not ask for this" rather
// than "there is nothing there".
//
// [Query.With] loads what it is given and nothing else. There is no lazy
// loading, so a loop over a slice cannot become a query per row. [Rel] carries
// the options — Where, OrderBy, a per-parent Limit — and [Rel.With] nests
// relations of the target to any depth. [Rel.Any] and [Rel.None] filter root
// rows by a relation without loading it.
//
// Loading is breadth-first and batched: the number of statements follows the
// shape of the requested tree and never the number of rows in it. PostgreSQL
// decides which rows relate, so citext, numeric, domains and composite keys
// behave as they do in the database rather than as Go equality would.
//
// # Transactions
//
// Generated code provides DB.Tx and DB.TxOptions over [RunTx] and
// [RunTxOptions]. The callback receives a DB bound to the transaction; the one
// it was called on is untouched. Returning nil commits, returning an error rolls
// back, and a panic rolls back and re-panics. Nothing is retried.
//
// # SQL composition
//
// A typed query is a typed source. [Sub] makes one a derived table, [CTE] makes
// one a WITH item, and [Compose] builds a statement over several of them with
// [ComposedQuery.Join] and its outer and lateral forms. Subqueries are [Exists],
// [InSub] and [Scalar]; [RecursiveCTE] walks a hierarchy. All of it nests
// through one compiler, so a statement with a CTE, a derived table, a
// correlated subquery and a window function has one parameter list, numbered in
// the order the SQL is written.
//
// Expressions cross into a composed query through [Of], [Opt] and [Cond], and
// what checks them there is source identity rather than an entity tag: a
// reference to an occurrence the statement does not introduce is refused, and
// the rules are sequential — a join condition sees the sources written before
// it and the one it attaches.
//
// Nullability is a property of the query as well as of the column. A value read
// through an outer join can be NULL whatever its constraint says, so it is read
// with [Opt] or [OptRef], which widen the result type; a select list that does
// not is refused rather than left to fail as a scan error. See
// docs/composition.md for the whole of it, including which mistakes the Go
// compiler catches, which the builder catches, and which are PostgreSQL's.
//
// # PostgreSQL's own types
//
// Where PostgreSQL has a type Go does not, the type is kept rather than reduced
// to something Go already has. [Range] and [Multirange] are generic over their
// element and carry the whole bound model — inclusive, exclusive, unbounded,
// empty — because a pair of endpoints cannot say which of those a value is;
// which of daterange, tsrange and tstzrange a Range[time.Time] column has is
// read from the catalog rather than guessed from Go. [Interval] keeps months,
// days and microseconds apart, and refuses to become a time.Duration when it
// holds a calendar component, because a month has no fixed length. Values that
// PostgreSQL canonicalises — discrete ranges, every multirange — come back as
// the server holds them. See docs/types.md.
//
// # Performance intelligence
//
// [Query.Explain] returns PostgreSQL's plan without running the statement;
// [Query.ExplainAnalyze] runs it to measure it, and the two names differ
// because the behaviours differ dangerously. [Query.Fingerprint] identifies a
// statement's shape independently of its values, [Query.Diagnostics] reads its
// structure without a database, and [Query.PerformanceReport] gathers all of it
// with a plain EXPLAIN — never an ANALYZE.
//
// None of it recommends anything. There is no index advisor and no tuning
// advice in this package: PostgreSQL plans, and what to change about a schema
// or a server needs a whole workload rather than one statement. See
// docs/performance.md.
//
// # Escape hatches
//
// [Expr] is a SQL fragment inside a query this package builds. [Raw] is a whole
// statement with the generated scanner kept. Both accept SQL text deliberately;
// neither accepts values formatted into it, and every value a caller passes
// becomes a parameter.
//
// # Errors
//
// Sentinels are wrapped rather than replaced, so errors.Is works through the
// context each layer adds, and PostgreSQL's own *pgconn.PgError stays reachable
// with errors.As. No exported API panics for a bad query, a database error or a
// failed scan.
//
// # Concurrency
//
// Generated descriptors and metadata are read-only and safe to share; every
// operation that looks like mutation — aliasing a table, configuring a relation
// — returns a copy. Builders are mutable and are not safe for concurrent use.
//
// # Dependency boundary
//
// Runtime packages may depend only on the standard library and
// github.com/jackc/pgx/v5. The generator and CLI may additionally depend on
// golang.org/x/tools and a YAML parser. The boundary is enforced by
// TestDependencyBoundary in this package's test suite.
package orm
