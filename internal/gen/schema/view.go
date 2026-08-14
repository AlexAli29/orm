package schema

import (
	"github.com/AlexAli29/orm/internal/gen/eligible"
	"slices"
	"strings"
)

// Views and materialized views.
//
// They are separate types rather than a Table with a kind on it, and that is
// the whole design decision. A view has no primary key, no foreign keys and
// nothing to COPY into; a materialized view has indexes but still no
// constraints. Modelling all three as one struct would mean every reader
// checking a kind before touching a field, and the first reader that forgot
// would be a migration planner writing ALTER TABLE against a view. Here the
// capability is the type: a View has no Indexes field to lose, and no code can
// ask a view for its primary key because there is nothing to ask.
//
// What they share with a table is columns, because that is what a query needs
// and there is no reason for a second column model.

// RelationKind is what a relation is.
//
// It exists so that a single name can be resolved to one of the three without
// the caller guessing, and so that a change of kind under one name is a thing
// the schema can state rather than a thing a diff has to infer.
type RelationKind uint8

const (
	// KindTable is an ordinary table.
	KindTable RelationKind = iota
	// KindView is an ordinary view: a stored SELECT, evaluated on every read.
	KindView
	// KindMaterializedView is a materialized view: a stored SELECT whose rows
	// are kept on disk and only change when it is refreshed.
	KindMaterializedView
)

// String returns the kind as PostgreSQL's documentation names it.
func (k RelationKind) String() string {
	switch k {
	case KindView:
		return "view"
	case KindMaterializedView:
		return "materialized view"
	default:
		return "table"
	}
}

// SQL returns the object type as it appears in DDL: TABLE, VIEW or
// MATERIALIZED VIEW.
func (k RelationKind) SQL() string {
	switch k {
	case KindView:
		return "VIEW"
	case KindMaterializedView:
		return "MATERIALIZED VIEW"
	default:
		return "TABLE"
	}
}

// RelationRef names a relation a definition depends on.
//
// It is a name and a kind, not a pointer, because the schema is a value: a
// dependency that held a pointer could not be copied, compared or diffed
// without aliasing whatever it pointed at.
type RelationRef struct {
	Schema string
	Name   string
	// Kind is what the dependency is, when it is known. Database-first
	// introspection reads it from the catalog; an explicitly declared
	// dependency may not name it, and KindTable is not assumed.
	Kind RelationKind
	// KindKnown separates "an ordinary table" from "nobody said".
	KindKnown bool
}

// Qualified renders the reference as schema.name.
func (r RelationRef) Qualified() string { return r.Schema + "." + r.Name }

// Definition is a view's SELECT statement.
//
// There are two texts here and the difference between them is the reason this
// is a struct rather than a string.
//
// SQL is what the project wrote, or what the generator compiled from a typed
// declaration. Canonical is what PostgreSQL says the same definition is, read
// back with pg_get_viewdef — which reconstructs the parsed query rather than
// returning the text anybody typed. The two are almost never equal byte for
// byte even when they mean exactly the same thing: PostgreSQL requalifies every
// name, expands *, adds casts, drops comments and reformats everything.
//
// Comparing them directly is therefore the wrong comparison, and normalising
// SQL text to make it right is a parser this project is not going to write with
// regular expressions. What is compared instead is documented on [Definition.Same].
type Definition struct {
	// SQL is the definition as the project states it: user-authored SQL, or SQL
	// the generator produced from a typed declaration. It is empty for a
	// relation read out of a database that the project does not declare.
	SQL string
	// Canonical is PostgreSQL's own reconstruction, from pg_get_viewdef. It is
	// empty for a desired schema that has never been near a server.
	//
	// It is never serialised. A migration file is committed and has to read the
	// same on every machine and against every supported PostgreSQL major, and
	// this text does not: 16's deparser stopped qualifying columns 15's
	// qualified, so writing it would make one project produce two artifacts
	// depending on which server generated them. Where it genuinely needs to be
	// stored — the per-database record of what was applied — it is written to
	// its own column by name, not carried along inside a schema value.
	//
	// It is not a normalised form of SQL and must never be treated as one: it
	// is a second, independent statement of the same definition, produced by
	// the only thing entitled to say what a definition means.
	Canonical string `json:"-"`
}

// Same reports whether two definitions are the same definition.
//
// This is server-canonical definition identity, and the name is exact. Two
// canonical texts come from one deparser over one parsed query, so comparing
// them sees through formatting: reindenting a definition, moving its
// whitespace, or adding a comment produces identical canonical text, and
// changing a predicate, a projection or a join does not. It is not a claim that
// two syntactically different formulations mean the same thing — nothing here
// proves equivalence, and nothing here normalises SQL.
//
// # Canonical text is per-server, and must not be committed
//
// The deparser is not stable across PostgreSQL majors. The same view yields
//
//	PostgreSQL 14, 15:   SELECT xt.id, xt.email FROM xt WHERE xt.active
//	PostgreSQL 16 to 18: SELECT id,    email    FROM xt WHERE active
//
// because 16 stopped qualifying columns it does not need to. Nothing about the
// view changed; the deparser did.
//
// So canonical text answers exactly one question: does this database still hold
// the definition the project declared? Both sides of that comparison come from
// the same server, so the deparser's version cancels out. It must never reach a
// lock file, a committed artifact or a migration's identity — an identical
// project checked out against 15 and against 16 would produce different bytes
// and churn every time somebody switched, which teaches people to regenerate
// until the diff goes away.
//
// When only the project's own text is available on both sides — two desired
// schemas, neither of which has been near a server — the texts are compared
// after collapsing whitespace, and nothing more. That is honest about what it
// is: a comparison of what was written, which cannot see that two different
// spellings mean the same thing. It is used only where the alternative is no
// comparison at all.
func (d Definition) Same(other Definition) bool {
	if d.Canonical != "" && other.Canonical != "" {
		return d.Canonical == other.Canonical
	}
	return collapseSpace(d.SQL) == collapseSpace(other.SQL)
}

// collapseSpace reduces runs of whitespace to one space and trims the ends.
//
// It is deliberately not a normaliser. It does not know that a comment is not
// code, that a string literal's spaces are data, or that two spellings of a
// name are one name — and it is only ever used when no canonical text exists on
// either side, where the alternative is comparing nothing.
func collapseSpace(s string) string { return strings.Join(strings.Fields(s), " ") }

// View is an ordinary view.
//
// It is readable and nothing else. There is no primary key, no constraint and
// no index here because PostgreSQL allows none of them on an ordinary view, and
// there is no write capability because M16.5 generates read-only view sources:
// PostgreSQL will accept writes through some views, and exposing that is a
// larger contract than this milestone signs.
type View struct {
	Schema string
	Name   string
	// Columns are in the order the view's SELECT produces them. Order is
	// meaning here even more than on a table: it is what CREATE OR REPLACE VIEW
	// is allowed to preserve and nothing else.
	Columns []Column
	// Definition is the SELECT the view stands for.
	Definition Definition
	// DependsOn are the relations the definition reads, sorted by qualified
	// name. They order migrations: a view cannot be created before what it
	// selects from.
	DependsOn []RelationRef
	// Options are the view options PostgreSQL records — security_barrier,
	// security_invoker, check_option. They are carried rather than interpreted:
	// dropping them on the way through introspection would silently change what
	// a view means, and this milestone does not add a security DSL.
	Options []ViewOption
}

// Qualified renders the view as schema.name.
func (v View) Qualified() string { return v.Schema + "." + v.Name }

// Column returns the named column and whether it exists.
func (v View) Column(name string) (Column, bool) { return findColumn(v.Columns, name) }

// ViewOption is one entry of a view's reloptions, or its check option.
//
// PostgreSQL stores these on the relation, and they change what the view means:
// security_invoker decides whose privileges the underlying tables are read
// with, and a check option decides whether a write through the view is allowed
// to produce a row the view cannot see. Reading a view and writing it back
// without them would quietly change a security boundary, which is why they are
// represented even though managing them is not part of this milestone.
type ViewOption struct {
	Name  string
	Value string
}

// MaterializedView is a materialized view.
//
// It is readable, refreshable and indexable, and it is none of the other things
// a table is: PostgreSQL allows no primary key, foreign key or check constraint
// on one, so there is nowhere here to put them.
type MaterializedView struct {
	Schema string
	Name   string
	// Columns are in the order the defining SELECT produces them.
	Columns []Column
	// Definition is the SELECT whose result is stored.
	Definition Definition
	// DependsOn are the relations the definition reads, sorted by qualified name.
	DependsOn []RelationRef
	// Indexes are ordinary PostgreSQL indexes, in the same model tables use.
	// There is no reduced index type for materialized views: an index on one is
	// an index, and a unique index on one is what REFRESH CONCURRENTLY needs.
	Indexes []Index
	// WithData says whether the schema asks for the view to be populated when
	// it is created. It is creation policy, not runtime state.
	//
	// The two are deliberately different things. A materialized view created
	// WITH NO DATA is unscannable until something refreshes it, and that is a
	// fact about the database at a moment, not about the schema — a schema that
	// stored it would report drift every time somebody refreshed.
	// [MaterializedView.Populated] is the runtime half.
	WithData bool
	// Populated is what the server currently says, and is runtime state rather
	// than schema identity: it is filled in by introspection and ignored by
	// diffing.
	Populated bool
	// Tablespace is where the view's storage lives, empty for the default.
	Tablespace string
}

// Qualified renders the materialized view as schema.name.
func (m MaterializedView) Qualified() string { return m.Schema + "." + m.Name }

// Column returns the named column and whether it exists.
func (m MaterializedView) Column(name string) (Column, bool) { return findColumn(m.Columns, name) }

// ConcurrentRefreshIndex returns the index that lets REFRESH CONCURRENTLY run,
// and whether there is one.
//
// PostgreSQL's requirement is exact, and all of it is visible in the schema: the
// view needs at least one unique index that covers every row and is built from
// plain column names — so not partial, and not over an expression. A unique
// index that fails any of those is not a candidate, and offering it would mean
// sending a REFRESH the server was always going to reject.
//
// This is a statement about the schema, not about the database now. The view
// must also be populated, which is runtime state nothing here can promise: see
// [MaterializedView.Populated], and expect the server to have the last word.
func (m MaterializedView) ConcurrentRefreshIndex() (Index, bool) {
	candidates := make([]eligible.Candidate, 0, len(m.Indexes))
	for _, ix := range m.Indexes {
		expression := false
		for _, c := range ix.Columns {
			if c.Expression != "" || c.Name == "" {
				expression = true
				break
			}
		}
		candidates = append(candidates, eligible.Candidate{
			Name: ix.Name, Unique: ix.Unique, Partial: ix.Where != "",
			Expression: expression, Columns: len(ix.Columns),
		})
	}
	name := eligible.Choose(candidates)
	if name == "" {
		return Index{}, false
	}
	for _, ix := range m.Indexes {
		if ix.Name == name {
			return ix, true
		}
	}
	return Index{}, false
}

func findColumn(cols []Column, name string) (Column, bool) {
	for _, c := range cols {
		if c.Name == name {
			return c, true
		}
	}
	return Column{}, false
}

// SortViews orders views by qualified name.
func SortViews(vs []View) {
	slices.SortFunc(vs, func(a, b View) int { return strings.Compare(a.Qualified(), b.Qualified()) })
}

// SortMaterializedViews orders materialized views by qualified name.
func SortMaterializedViews(ms []MaterializedView) {
	slices.SortFunc(ms, func(a, b MaterializedView) int {
		return strings.Compare(a.Qualified(), b.Qualified())
	})
}

// SortRefs orders dependency references by qualified name.
func SortRefs(rs []RelationRef) {
	slices.SortFunc(rs, func(a, b RelationRef) int { return strings.Compare(a.Qualified(), b.Qualified()) })
}

// Clone returns a deep copy of a view.
func (v View) Clone() View {
	out := v
	out.Columns = slices.Clone(v.Columns)
	out.DependsOn = slices.Clone(v.DependsOn)
	out.Options = slices.Clone(v.Options)
	return out
}

// Clone returns a deep copy of a materialized view.
func (m MaterializedView) Clone() MaterializedView {
	out := m
	out.Columns = slices.Clone(m.Columns)
	out.DependsOn = slices.Clone(m.DependsOn)
	out.Indexes = make([]Index, 0, len(m.Indexes))
	for _, i := range m.Indexes {
		i.Columns = slices.Clone(i.Columns)
		i.Include = slices.Clone(i.Include)
		out.Indexes = append(out.Indexes, i)
	}
	return out
}
