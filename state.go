package orm

import (
	"bytes"
	"encoding/json"
	"fmt"
)

// jsonNull is the literal a relation encodes to when it carries no rows.
var jsonNull = []byte("null")

// Many holds the to-many side of a relation.
//
// A Many distinguishes three states, which is the whole reason it exists:
//
//	Many[T]{}      unloaded — the relation was never fetched
//	NewMany[T]()   loaded, zero related rows
//	NewMany(a, b)  loaded, two related rows
//
// The zero value is unloaded, so a struct literal that omits the field says
// "I did not ask for this" rather than "there is nothing there". Because
// [Many.IsZero] reports true exactly when unloaded, the encoding/json omitzero
// option drops unloaded relations from the output:
//
//	Posts orm.Many[Post] `json:"posts,omitzero"`
//
// A loaded but empty Many encodes as [], never as null.
type Many[T any] struct {
	items  []T
	loaded bool
}

// NewMany returns a loaded Many holding vs. NewMany with no arguments returns a
// loaded, empty relation, which is distinct from the unloaded zero value.
func NewMany[T any](vs ...T) Many[T] {
	if vs == nil {
		vs = []T{}
	}
	return Many[T]{items: vs, loaded: true}
}

// NewManyFrom returns a loaded Many holding vs. A nil slice yields a loaded,
// empty relation rather than an unloaded one.
func NewManyFrom[T any](vs []T) Many[T] {
	return NewMany(vs...)
}

// Get returns the related rows and whether the relation was loaded. The returned
// slice aliases the relation's storage; it is not copied.
func (m Many[T]) Get() ([]T, bool) {
	if !m.loaded {
		return nil, false
	}
	return m.items, true
}

// MustGet returns the related rows and panics if the relation was never loaded.
// The panic reports a programming error — a caller reading a relation it did not
// request — not a data condition.
func (m Many[T]) MustGet() []T {
	if !m.loaded {
		panic(fmt.Sprintf("orm: Many[%T] is not loaded", *new(T)))
	}
	return m.items
}

// Len returns the number of related rows, or 0 when the relation is unloaded.
// Len alone cannot distinguish the two; use [Many.Get] or [Many.IsZero].
func (m Many[T]) Len() int { return len(m.items) }

// IsZero reports whether the relation is unloaded. It implements the contract
// that encoding/json's omitzero option looks for.
func (m Many[T]) IsZero() bool { return !m.loaded }

// MarshalJSON encodes a loaded relation as a JSON array — [] when empty — and an
// unloaded relation as null. Unloaded relations are normally omitted entirely by
// tagging the field with omitzero; null is the fallback when they are not.
func (m Many[T]) MarshalJSON() ([]byte, error) {
	if !m.loaded {
		return jsonNull, nil
	}
	return json.Marshal(m.items)
}

// UnmarshalJSON decodes a JSON array into a loaded relation and JSON null into
// the unloaded zero value, inverting [Many.MarshalJSON].
func (m *Many[T]) UnmarshalJSON(data []byte) error {
	if isJSONNull(data) {
		*m = Many[T]{}
		return nil
	}
	var vs []T
	if err := json.Unmarshal(data, &vs); err != nil {
		return fmt.Errorf("orm: decoding Many[%T]: %w", *new(T), err)
	}
	*m = NewManyFrom(vs)
	return nil
}

// One holds the to-one side of a relation.
//
// It distinguishes three states:
//
//	One[T]{}       unloaded — the relation was never fetched
//	Absent[T]()    loaded, no related row exists
//	NewOne(&v)     loaded, related row v
//
// As with [Many], the zero value is unloaded and [One.IsZero] drives omitzero.
// A loaded-but-absent relation encodes as null.
type One[T any] struct {
	v      *T
	loaded bool
}

// NewOne returns a loaded One referring to v. NewOne(nil) is equivalent to
// [Absent].
func NewOne[T any](v *T) One[T] { return One[T]{v: v, loaded: true} }

// Absent returns a loaded One that carries no related row. It is what a
// belongs-to relation holds when its foreign key is NULL, and what a has-one
// relation holds when the counterpart row does not exist.
func Absent[T any]() One[T] { return One[T]{loaded: true} }

// Get returns the related row and whether the relation was loaded. A loaded
// relation with no related row returns (nil, true).
func (o One[T]) Get() (*T, bool) {
	if !o.loaded {
		return nil, false
	}
	return o.v, true
}

// MustGet returns the related row and panics if the relation was never loaded.
// A loaded but absent relation returns nil without panicking.
func (o One[T]) MustGet() *T {
	if !o.loaded {
		panic(fmt.Sprintf("orm: One[%T] is not loaded", *new(T)))
	}
	return o.v
}

// IsZero reports whether the relation is unloaded, driving encoding/json's
// omitzero option.
func (o One[T]) IsZero() bool { return !o.loaded }

// MarshalJSON encodes the related row, or null when the relation is loaded but
// absent. An unloaded relation also encodes as null; tag the field with omitzero
// to omit it instead.
func (o One[T]) MarshalJSON() ([]byte, error) {
	if !o.loaded || o.v == nil {
		return jsonNull, nil
	}
	return json.Marshal(o.v)
}

// UnmarshalJSON decodes a JSON object into a loaded relation and JSON null into
// a loaded, absent relation. Note the asymmetry with [Many.UnmarshalJSON]: for a
// to-one relation null is a meaningful loaded value, so it cannot also mean
// unloaded. An unloaded One is produced by the field being absent from the JSON
// document, which leaves the zero value untouched.
func (o *One[T]) UnmarshalJSON(data []byte) error {
	if isJSONNull(data) {
		*o = Absent[T]()
		return nil
	}
	v := new(T)
	if err := json.Unmarshal(data, v); err != nil {
		return fmt.Errorf("orm: decoding One[%T]: %w", *new(T), err)
	}
	*o = NewOne(v)
	return nil
}

// isJSONNull reports whether data is the JSON null literal, ignoring the
// surrounding whitespace that encoding/json permits.
func isJSONNull(data []byte) bool {
	return bytes.Equal(bytes.TrimSpace(data), jsonNull)
}
