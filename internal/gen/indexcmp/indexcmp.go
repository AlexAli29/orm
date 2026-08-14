// Package indexcmp owns one rule: when are two PostgreSQL indexes the same
// index?
//
// It exists because the rule was written twice. The migration planner had a
// copy, deciding whether makemigrations writes a migration for an index; the
// schema comparison had another, deciding whether orm check reports drift and
// whether the declarations and the migrations are reported as agreeing. The two
// agreed, and nothing made them agree — a mutation campaign found that removing
// GiST from one left every test of the other green, so a project's drift check
// would stop reporting a hand-edited GiST index while planning carried on
// working perfectly.
//
// That failure is silent in exactly one direction at a time, which is the worst
// shape it could have. Whichever half went quiet, the other keeps behaving, so
// nothing looks broken: either a user is never told about an index somebody
// changed by hand, or they are never offered the migration for one they
// declared.
//
// So the rule lives here, in a package with no dependencies, and both callers
// pass their indexes through it. Neither can restate it without adding
// obviously new code in an obviously wrong place. This is the same answer the
// eligibility rule got, for the same reason.
//
// # What the rule is
//
// Two indexes are the same when everything PostgreSQL would have to rebuild
// them to change is the same: uniqueness, access method, the partial predicate,
// the covering columns, and the key list compared as a sequence — each key's
// column or expression, its direction, its nulls ordering and its operator
// class. Key order is part of it because (a, b) and (b, a) answer different
// queries.
//
// # What the rule is not
//
// It is not the index's name. Callers pair indexes by name before asking, and
// what they do when a name is on one side only — plan a create, plan a drop,
// report a missing index, report an extra one — is theirs to decide. A renamed
// index is a different question from a changed one.
//
// It is not which relation the index belongs to. Resolving an owner is the
// caller's, and it is a question with its own failure modes: a name without its
// schema resolves to a namesake in another schema, which is a defect about
// identity rather than about equality.
//
// It is not whether the index was built CONCURRENTLY. That is how an index was
// created, not what it is: two otherwise identical indexes are the same index
// whichever way each was built, and rebuilding one to change that would achieve
// nothing.
//
// It is not expression equivalence. Nothing here parses SQL. Predicates and
// expression keys arrive already reduced to whatever normal form the caller's
// own expression comparison uses, because that comparison has to answer the
// same question for defaults and check constraints too and must not be
// duplicated here either.
package indexcmp

// Index is an index reduced to what decides its identity.
//
// Every field is compared. That is deliberate: a shape carrying something the
// rule ignores would be a place for a reader to wonder which fields matter, and
// the answer would live in two places again. Anything not part of index
// identity — the name, the owner, how it was built — does not appear.
type Index struct {
	// Unique reports a unique index.
	Unique bool
	// Method is the access method, already defaulted: an empty method means
	// btree, and the caller resolves that before comparing so that a
	// declaration naming no method and a catalog reporting btree are the same
	// index rather than differing forever.
	Method string
	// Where is the partial predicate, already reduced to the caller's normal
	// form. Empty means the index covers every row.
	Where string
	// Include are the covering columns, in order. They are stored in the index
	// and are not part of the key.
	Include []string
	// Keys are the key columns, in order. The order is part of the index.
	Keys []Key
}

// Key is one key of an index.
type Key struct {
	// Name is the column, empty when the key is an expression.
	Name string
	// Expression is the key's expression, already reduced to the caller's
	// normal form, and empty when the key is a plain column.
	Expression string
	// Direction is ASC or DESC, in whatever encoding the caller uses. It is
	// compared, never interpreted.
	Direction int
	// Nulls is NULLS FIRST or NULLS LAST, in the caller's encoding.
	Nulls int
	// OpClass is a non-default operator class, such as text_pattern_ops.
	OpClass string
}

// Equal reports whether two indexes are the same index.
func Equal(a, b Index) bool {
	if a.Unique != b.Unique || a.Method != b.Method || a.Where != b.Where {
		return false
	}
	if len(a.Include) != len(b.Include) {
		return false
	}
	for i := range a.Include {
		if a.Include[i] != b.Include[i] {
			return false
		}
	}
	if len(a.Keys) != len(b.Keys) {
		return false
	}
	// By position rather than by set. An index over (user_id, created_at) can
	// serve a query filtering on user_id alone and one over (created_at,
	// user_id) cannot, so they are different indexes.
	for i := range a.Keys {
		if a.Keys[i] != b.Keys[i] {
			return false
		}
	}
	return true
}
