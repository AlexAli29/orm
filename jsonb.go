package orm

import (
	"github.com/AlexAli29/orm/internal/expr"
)

// JSONB.
//
// PostgreSQL's jsonb operators are exposed as themselves rather than wrapped in
// a document-database abstraction: -> is ->, @> is @>, and jsonb_set is
// jsonb_set. What this package adds is the typing — which result is text and
// which is jsonb, and which of them can be NULL.
//
// The type parameter J is the Go type the jsonb column maps to, whatever the
// project chose: a map, a struct, a slice, []byte. Nothing here decodes it, so
// nothing here has an opinion about it. Handing one of these an expression that
// is not jsonb is an error PostgreSQL reports precisely, naming the operator
// and the types.
//
// # Three kinds of nothing
//
// PostgreSQL distinguishes three things Go tends to blur:
//
//	SQL NULL      the column itself has no value
//	JSON null     the document has the key, and its value is the JSON null
//	missing key   the document does not have the key
//
// The operators keep them apart, and so does this: -> and ->> return SQL NULL
// for a missing key and a JSON null for a key whose value is one, and HasKey
// answers TRUE for a key whose value is JSON null. The permanent test suite
// checks each of the five cases against PostgreSQL rather than against a
// comment.

// JSONGet reads a top-level key as jsonb.
//
// The result is nullable and stays jsonb: a missing key is SQL NULL, and a key
// whose value is the JSON null is a jsonb document holding null — which is not
// the same thing, and which only a jsonb-typed result can tell apart from the
// first.
func JSONGet[E, J any](doc Optional[E, *J], key string) Expression[*J, *J] {
	return jsonExpr[J](doc, "->", expr.Arg{Value: key})
}

// JSONIndex reads an array element by position, counting from zero. A negative
// index counts from the end, as PostgreSQL's does.
func JSONIndex[E, J any](doc Optional[E, *J], i int32) Expression[*J, *J] {
	return jsonExpr[J](doc, "->", jsonIndexArg(i))
}

// JSONIndexText reads an array element as text.
func JSONIndexText[E, J any](doc Optional[E, *J], i int32) Expression[*string, *string] {
	return Expression[*string, *string]{
		node:     expr.Infix{Op: "->>", Left: doc.optItem().Node, Right: jsonIndexArg(i)},
		nullSafe: true,
	}
}

// jsonIndexArg binds an array index and says it is an integer.
//
// PostgreSQL has two -> operators and two ->> operators: one taking a text key
// and one taking an integer subscript. A bare parameter carries no type, so the
// server resolves it to text and the integer form is never reached — the
// statement then fails to encode rather than reading the element. The cast is
// what picks the operator, and the index is still a value.
func jsonIndexArg(i int32) expr.Node {
	return expr.Cast{X: expr.Arg{Value: i}, Type: "int4"}
}

// JSONPathGet reads a nested value as jsonb, following PostgreSQL's #> .
//
// The path travels as a text[] bind parameter rather than as assembled SQL, so
// a key containing a quote, a brace or a comma is a key rather than a syntax
// problem.
//
//	orm.JSONPathGet(Users.Settings, "profile", "address")
func JSONPathGet[E, J any](doc Optional[E, *J], path ...string) Expression[*J, *J] {
	return jsonExpr[J](doc, "#>", expr.Arg{Value: path})
}

// JSONPathText reads a nested value as text, following PostgreSQL's #>> .
//
//	orm.JSONPathText(Users.Settings, "profile", "age")
func JSONPathText[E, J any](doc Optional[E, *J], path ...string) Expression[*string, *string] {
	return Expression[*string, *string]{
		node:     expr.Infix{Op: "#>>", Left: doc.optItem().Node, Right: expr.Arg{Value: path}},
		nullSafe: true,
	}
}

// JSONContainedBy builds a <@ b, which is TRUE when a is a subdocument of b.
func JSONContainedBy[A, B, J any](a Optional[A, *J], b Optional[B, *J]) Predicate[Composed] {
	return Predicate[Composed]{node: expr.Infix{
		Op: "<@", Left: a.optItem().Node, Right: b.optItem().Node,
	}}
}

// JSONHasAnyKeys builds doc ?| keys, which is TRUE when the document has at
// least one of them at the top level.
func JSONHasAnyKeys[E, J any](doc Optional[E, *J], keys ...string) Predicate[Composed] {
	return Predicate[Composed]{node: expr.Infix{
		Op: "?|", Left: doc.optItem().Node, Right: expr.Arg{Value: keys},
	}}
}

// JSONHasAllKeys builds doc ?& keys, which is TRUE when the document has every
// one of them at the top level.
func JSONHasAllKeys[E, J any](doc Optional[E, *J], keys ...string) Predicate[Composed] {
	return Predicate[Composed]{node: expr.Infix{
		Op: "?&", Left: doc.optItem().Node, Right: expr.Arg{Value: keys},
	}}
}

// JSONSet returns the document with the value at the path replaced.
//
//	orm.JSONSet(Users.Settings, []string{"profile", "verified"}, verified)
//
// It is an ordinary expression, so it composes wherever one does — a select
// list, an UPDATE's SetExpr, a RETURNING clause. The path is a text[] parameter
// and the new value is a jsonb parameter; neither is assembled as text.
//
// createMissing is PostgreSQL's fourth argument: with it, a path that does not
// exist is created, and without it such a call leaves the document alone.
func JSONSet[E, J any](doc Selectable[E, J], path []string, value J, createMissing bool) Value[E, J] {
	return Value[E, J]{node: jsonSetNode("jsonb_set", doc.selectItem().Node, path, value, createMissing)}
}

// JSONSetNull is [JSONSet] over a nullable document, which stays nullable:
// jsonb_set of a NULL document is NULL.
func JSONSetNull[E, J any](doc Selectable[E, *J], path []string, value J, createMissing bool) Value[E, *J] {
	return Value[E, *J]{
		node:     jsonSetNode("jsonb_set", doc.selectItem().Node, path, value, createMissing),
		nullable: true,
	}
}

// JSONInsert returns the document with a value inserted at the path.
//
// after chooses PostgreSQL's insert_after: with it the value goes after the
// element the path names rather than before it.
//
// It inserts rather than replaces, and PostgreSQL is strict about the
// difference: a path that already exists is an error rather than a no-op, and
// the error arrives as a *pgconn.PgError. [JSONSet] is the one that replaces.
func JSONInsert[E, J any](doc Selectable[E, J], path []string, value J, after bool) Value[E, J] {
	return Value[E, J]{node: jsonSetNode("jsonb_insert", doc.selectItem().Node, path, value, after)}
}

// JSONInsertNull is [JSONInsert] over a nullable document.
func JSONInsertNull[E, J any](doc Selectable[E, *J], path []string, value J, after bool) Value[E, *J] {
	return Value[E, *J]{
		node:     jsonSetNode("jsonb_insert", doc.selectItem().Node, path, value, after),
		nullable: true,
	}
}

// jsonSetNode builds the two four-argument modification calls, whose shape is
// the same: the document, a text[] path, a jsonb value, and a flag.
func jsonSetNode(fn string, doc expr.Node, path []string, value any, flag bool) expr.Node {
	return expr.Call{Func: fn, Args: []expr.Node{
		doc,
		expr.Arg{Value: path},
		expr.Arg{Value: value},
		expr.Arg{Value: flag},
	}}
}

// JSONStripNulls returns the document with every object key whose value is the
// JSON null removed. Array nulls are left alone, which is PostgreSQL's rule.
func JSONStripNulls[E, J any](doc Selectable[E, J]) Value[E, J] {
	return Value[E, J]{
		node: expr.Call{Func: "jsonb_strip_nulls", Args: []expr.Node{doc.selectItem().Node}},
	}
}

// JSONStripNullsOf is [JSONStripNulls] over a nullable document.
func JSONStripNullsOf[E, J any](doc Selectable[E, *J]) Value[E, *J] {
	return Value[E, *J]{
		node:     expr.Call{Func: "jsonb_strip_nulls", Args: []expr.Node{doc.selectItem().Node}},
		nullable: true,
	}
}

// JSONArrayLength is the number of elements in a jsonb array.
//
// It is nullable because the document may be, and it is an error rather than a
// NULL when the document is not an array — which is PostgreSQL's answer, not
// something this package softens.
func JSONArrayLength[E, J any](doc Optional[E, *J]) Expression[*int32, *int32] {
	return Expression[*int32, *int32]{
		node:     expr.Call{Func: "jsonb_array_length", Args: []expr.Node{doc.optItem().Node}},
		nullSafe: true,
	}
}

// JSONTypeOf is PostgreSQL's jsonb_typeof: one of object, array, string,
// number, boolean or null, as text.
func JSONTypeOf[E, J any](doc Optional[E, *J]) Expression[*string, *string] {
	return Expression[*string, *string]{
		node:     expr.Call{Func: "jsonb_typeof", Args: []expr.Node{doc.optItem().Node}},
		nullSafe: true,
	}
}

// JSONMatches builds doc @@ jsonpath, which is TRUE when the JSONPath
// predicate holds for the document.
//
// # The path is JSONPath, not SQL
//
// The string is PostgreSQL's JSONPath language and is bound as a value, so it
// never becomes part of the statement's syntax. What it is not is checked: this
// package does not parse JSONPath, so a malformed one is an error PostgreSQL
// reports. That is the same trust boundary [RawValue] has, narrowed to one
// argument of one operator.
func JSONMatches[E, J any](doc Optional[E, *J], path string) Predicate[Composed] {
	return Predicate[Composed]{node: expr.Infix{
		Op: "@@", Left: doc.optItem().Node, Right: jsonPath(path),
	}}
}

// JSONPathExists builds doc @? jsonpath, which is TRUE when the JSONPath
// expression selects anything.
func JSONPathExists[E, J any](doc Optional[E, *J], path string) Predicate[Composed] {
	return Predicate[Composed]{node: expr.Infix{
		Op: "@?", Left: doc.optItem().Node, Right: jsonPath(path),
	}}
}

// jsonPath binds a JSONPath as a value and casts it to PostgreSQL's jsonpath
// type, which is what the two operators take. The cast is written by this
// package; the path is a parameter.
func jsonPath(path string) expr.Node {
	return expr.Cast{X: expr.Arg{Value: path}, Type: "jsonpath"}
}

func jsonExpr[J any, E any](doc Optional[E, *J], op string, right expr.Node) Expression[*J, *J] {
	return Expression[*J, *J]{
		node:     expr.Infix{Op: op, Left: doc.optItem().Node, Right: right},
		nullSafe: true,
	}
}
