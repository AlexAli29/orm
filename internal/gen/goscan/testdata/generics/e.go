// Package generics exercises the scanner's handling of instantiated generics.
package generics

import (
	"time"

	"github.com/AlexAli29/orm"
)

// Box is a generic whose instantiations must stay distinguishable.
type Box[T any] struct {
	V     T
	Valid bool
}

// Pair nests one generic inside another.
type Pair[A any, B any] struct {
	First  A
	Second B
}

// Alias is a Go alias, which go/types resolves away entirely.
type Alias = Box[int32]

// Defined is a defined type over an instantiation, which keeps its own identity.
type Defined Box[int32]

//orm:table things
type Thing struct {
	ID     int64
	Ints   Box[int32]
	Longs  Box[int64]
	Strs   Box[string]
	Times  Box[time.Time]
	Nested Box[Box[int32]]
	Two    Pair[int32, string]
	// The same two arguments the other way round. Sorting them would make
	// these one type.
	TwoSwapped Pair[string, int32]
	PtrInts    *Box[int32]
	Aliased    Alias
	Distinct   Defined
	Plain      string
}

// The instantiations M12.2 depends on staying distinct. They are declared with
// the real types rather than with stand-ins, because the property being checked
// is about these exact ones.
//
//orm:table shapes
type Shapes struct {
	ID            int64
	RangeInt32    orm.Range[int32]
	RangeInt64    orm.Range[int64]
	RangeTime     orm.Range[time.Time]
	MultiInt32    orm.Multirange[int32]
	MultiInt64    orm.Multirange[int64]
	MultiTime     orm.Multirange[time.Time]
	NullRangeI32  *orm.Range[int32]
	NestedBoxed   Box[orm.Range[int32]]
	NestedBoxed64 Box[orm.Range[int64]]
	Interval      orm.Interval
}
