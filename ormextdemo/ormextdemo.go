// Package ormextdemo is a worked example of a third-party PostgreSQL extension
// built on the ORM's public SDK.
//
// It exists to prove something rather than to be useful: that an extension can
// be written entirely against the exported API, with no access to the AST, the
// writer, the scope checker or anything else under internal/.
//
// Its module path is example.com/ormextdemo rather than something under
// github.com/AlexAli29/orm, and that detail is the proof rather than decoration.
// Go's internal rule is about import paths, not modules: a module named
// github.com/AlexAli29/orm/ormextdemo would sit lexically inside the ORM and
// the compiler would happily let it import internal/expr. Naming it as a
// genuine third party is what makes the compiler refuse — and there is a test
// that keeps the guarantee honest either way.
//
// What it wraps is deliberately unexciting: functions and operators PostgreSQL
// already has, so that running its tests needs no CREATE EXTENSION and no image
// with anything special in it. The point is the shape of the integration, not
// the SQL.
//
// # What the SDK gives an extension
//
// Everything here is built from four public pieces:
//
//	orm.Arg          an operand, opaque by design
//	orm.ArgOf/Opt    a typed expression or its nullable form, as an operand
//	orm.ArgValue     a Go value, which becomes a bind parameter
//	orm.FnExpr/OpPredicate/...   a call or an operator over those operands
//
// An extension names a function and describes a shape. It never writes SQL text
// containing a caller's value, because there is no way to: [orm.ArgValue] binds.
//
// # What the extension is trusted with
//
// The result type and the nullability an extension declares are the extension's
// claim, not something the ORM verifies. FnExpr[string] says "PostgreSQL returns
// text here" and the ORM believes it; if the function actually returns an
// integer, the mistake surfaces as a scan error rather than a wrong value, but
// it is still the extension's mistake. That boundary is documented in
// docs/extensions.md and the conformance suite is how an author checks their
// side of it.
package ormextdemo

import (
	"github.com/AlexAli29/orm"
)

// MD5 is PostgreSQL's md5(text): the hash, as hexadecimal text.
//
// The declared result type is the extension's claim about what PostgreSQL
// returns. A conformance test compares it against pg_typeof, which is how the
// claim is checked rather than assumed.
func MD5[E any](v orm.Selectable[E, string]) orm.Expression[string, *string] {
	return orm.FnExpr[string]("md5", orm.ArgOf(v))
}

// MD5Null is [MD5] over a column that may be NULL.
//
// It is a separate function rather than an option because the result type
// differs: md5(NULL) is NULL, so the Go type has to be *string. Collapsing the
// two would mean one of them lying about what a row can hold.
func MD5Null[E any](v orm.Selectable[E, *string]) orm.Expression[*string, *string] {
	return orm.FnExprNull[string]("md5", orm.ArgOf(v))
}

// OctetLength is PostgreSQL's octet_length(text): the size in bytes, which for
// anything outside ASCII differs from the number of characters.
func OctetLength[E any](v orm.Selectable[E, string]) orm.Expression[int32, *int32] {
	return orm.FnExpr[int32]("octet_length", orm.ArgOf(v))
}

// Repeat is PostgreSQL's repeat(text, int): the string, n times.
//
// It takes its count as a Go value, which becomes a bind parameter like every
// other value the ORM sends. An extension has no way to splice one into the
// statement, which is the property that makes the SDK safe to hand to a
// third party.
func Repeat[E any](v orm.Selectable[E, string], n int32) orm.Expression[string, *string] {
	return orm.FnExpr[string]("repeat", orm.ArgOf(v), orm.ArgValue(n))
}

// StartsWith builds the ^@ operator: the left string begins with the right one.
//
// It is a predicate rather than a value, so it composes into a WHERE clause with
// the entity tag intact — an extension gets the ORM's source and entity checking
// without implementing any of it.
func StartsWith[E any](v orm.Selectable[E, string], prefix string) orm.Predicate[E] {
	return orm.OpPredicate[E]("^@", orm.ArgOf(v), orm.ArgValue(prefix))
}

// SameHash builds md5(a) = md5(b) across two columns.
//
// It exists to exercise the case that matters most for correctness: an operand
// that is itself an extension expression, on both sides, each carrying its own
// source. Whether the ORM still sees both sources — and still numbers the
// placeholders correctly around them — is what the conformance suite checks.
func SameHash[E any](a, b orm.Selectable[E, string]) orm.Predicate[E] {
	return orm.OpPredicate[E]("=",
		orm.ArgOf(MD5(a)),
		orm.ArgOf(MD5(b)))
}

// Tagged builds repeat(left, n) = right, an operator over two extension calls
// with a bind parameter buried inside one of them.
//
// It is the placeholder-numbering case: the value inside the nested call has to
// take its number from the statement's own namespace, wherever the expression
// ends up.
func Tagged[E any](v orm.Selectable[E, string], n int32, want string) orm.Predicate[E] {
	return orm.OpPredicate[E]("=",
		orm.ArgOf(Repeat(v, n)),
		orm.ArgValue(want))
}
