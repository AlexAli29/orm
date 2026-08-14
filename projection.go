package orm

import (
	"fmt"

	"github.com/AlexAli29/orm/internal/expr"
)

// Result shapes.
//
// Go cannot express "a list of expressions whose result types are all different
// and all remembered". A variadic parameter has one type; a type-parameter pack
// does not exist. Every library that pretends otherwise does it with []any and
// runtime assertions, which moves the mistake from the compiler to the customer.
//
// So the arity is written out. ProjectN takes N expressions with N independent
// type parameters and a function of exactly those N types, and returns the
// shape that scans them. The compiler checks the whole chain: the expressions
// decide the parameter types, the function decides the result type, and a
// column of the wrong type — or a nullable column bound to a destination that
// cannot hold NULL — does not compile.
//
// What that costs is a family of near-identical constructors. What it buys is
// that the row hot path does no reflection, holds no map, and asserts nothing:
// scanning is N typed locals, one Scan, and one call.

// Projection is a typed result shape: what to select, and how to read it back.
//
// It is a value, so one shape is built once and used by many queries:
//
//	var UserSummaries = orm.Project2(
//	    Users.ID, Users.Email,
//	    func(id int64, email string) UserSummary { return UserSummary{ID: id, Email: email} },
//	)
//
// A Projection is immutable and safe to share. The scanning state is not part
// of it: newScan hands each query its own, which is what lets one shape be
// read by several queries at once.
type Projection[E, R any] struct {
	items []expr.SelectItem
	// newScan returns a scanner owning the typed locals it reads into, so the
	// destinations are allocated once per query rather than once per row.
	newScan func() func(pgxRows) (R, error)
	// shape describes the result positionally: one slot per item, carrying the
	// Go type the scanner reads into and whether it can hold a NULL.
	//
	// R alone cannot answer that. Two projections can build the same struct out
	// of different columns, so a set operation comparing result types would
	// accept a branch feeding a nullable column into a destination the other
	// branch proved non-null. The slots are built from the same Selectable
	// values the items are, in the same order, by one helper — so an item and
	// its slot cannot describe different things.
	shape resultShape
}

// Columns reports how many expressions the shape selects.
func (p Projection[E, R]) Columns() int { return len(p.items) }

// validate reports the ways a shape could be unusable, before a statement is
// built from it.
func (p Projection[E, R]) validate() error {
	switch {
	case len(p.items) == 0:
		return fmt.Errorf("projection selects nothing")
	case p.newScan == nil:
		return fmt.Errorf("projection has no scanner")
	case len(p.shape.slots) != len(p.items):
		// A constructor that filled one and not the other would produce a shape
		// silently describing the wrong columns, which is exactly what a set
		// operation would then compare.
		return fmt.Errorf("projection describes %d result slots for %d expressions", len(p.shape.slots), len(p.items))
	}
	for i, it := range p.items {
		if it.Node == nil {
			return fmt.Errorf("projection expression %d is empty", i+1)
		}
	}
	// The names are the shape's business, and asking it is what keeps one rule
	// about them rather than one per place that has a list of names.
	return uniqueOutputNames(p.shape.slots)
}

func items(list ...expr.SelectItem) []expr.SelectItem { return list }

// Project1 builds a one-expression result shape.
func Project1[E, A, R any](a Selectable[E, A], build func(A) R) Projection[E, R] {
	return Projection[E, R]{
		items: items(a.selectItem()),
		shape: shapeOf(slotOf(a)),
		newScan: func() func(pgxRows) (R, error) {
			var va A
			dest := []any{&va}
			return func(rows pgxRows) (R, error) {
				if err := rows.Scan(dest...); err != nil {
					var zero R
					return zero, err
				}
				return build(va), nil
			}
		},
	}
}

// Project2 builds a two-expression result shape.
func Project2[E, A, B, R any](a Selectable[E, A], b Selectable[E, B],
	build func(A, B) R,
) Projection[E, R] {
	return Projection[E, R]{
		items: items(a.selectItem(), b.selectItem()),
		shape: shapeOf(slotOf(a), slotOf(b)),
		newScan: func() func(pgxRows) (R, error) {
			var (
				va A
				vb B
			)
			dest := []any{&va, &vb}
			return func(rows pgxRows) (R, error) {
				if err := rows.Scan(dest...); err != nil {
					var zero R
					return zero, err
				}
				return build(va, vb), nil
			}
		},
	}
}

// Project3 builds a three-expression result shape.
func Project3[E, A, B, C, R any](a Selectable[E, A], b Selectable[E, B], c Selectable[E, C],
	build func(A, B, C) R,
) Projection[E, R] {
	return Projection[E, R]{
		items: items(a.selectItem(), b.selectItem(), c.selectItem()),
		shape: shapeOf(slotOf(a), slotOf(b), slotOf(c)),
		newScan: func() func(pgxRows) (R, error) {
			var (
				va A
				vb B
				vc C
			)
			dest := []any{&va, &vb, &vc}
			return func(rows pgxRows) (R, error) {
				if err := rows.Scan(dest...); err != nil {
					var zero R
					return zero, err
				}
				return build(va, vb, vc), nil
			}
		},
	}
}

// Project4 builds a four-expression result shape.
func Project4[E, A, B, C, D, R any](a Selectable[E, A], b Selectable[E, B], c Selectable[E, C], d Selectable[E, D],
	build func(A, B, C, D) R,
) Projection[E, R] {
	return Projection[E, R]{
		items: items(a.selectItem(), b.selectItem(), c.selectItem(), d.selectItem()),
		shape: shapeOf(slotOf(a), slotOf(b), slotOf(c), slotOf(d)),
		newScan: func() func(pgxRows) (R, error) {
			var (
				va A
				vb B
				vc C
				vd D
			)
			dest := []any{&va, &vb, &vc, &vd}
			return func(rows pgxRows) (R, error) {
				if err := rows.Scan(dest...); err != nil {
					var zero R
					return zero, err
				}
				return build(va, vb, vc, vd), nil
			}
		},
	}
}

// Project5 builds a five-expression result shape.
func Project5[E, A, B, C, D, F, R any](a Selectable[E, A], b Selectable[E, B], c Selectable[E, C], d Selectable[E, D], f Selectable[E, F],
	build func(A, B, C, D, F) R,
) Projection[E, R] {
	return Projection[E, R]{
		items: items(a.selectItem(), b.selectItem(), c.selectItem(), d.selectItem(), f.selectItem()),
		shape: shapeOf(slotOf(a), slotOf(b), slotOf(c), slotOf(d), slotOf(f)),
		newScan: func() func(pgxRows) (R, error) {
			var (
				va A
				vb B
				vc C
				vd D
				vf F
			)
			dest := []any{&va, &vb, &vc, &vd, &vf}
			return func(rows pgxRows) (R, error) {
				if err := rows.Scan(dest...); err != nil {
					var zero R
					return zero, err
				}
				return build(va, vb, vc, vd, vf), nil
			}
		},
	}
}

// Project6 builds a six-expression result shape.
func Project6[E, A, B, C, D, F, G, R any](a Selectable[E, A], b Selectable[E, B], c Selectable[E, C], d Selectable[E, D], f Selectable[E, F], g Selectable[E, G],
	build func(A, B, C, D, F, G) R,
) Projection[E, R] {
	return Projection[E, R]{
		items: items(a.selectItem(), b.selectItem(), c.selectItem(), d.selectItem(), f.selectItem(), g.selectItem()),
		shape: shapeOf(slotOf(a), slotOf(b), slotOf(c), slotOf(d), slotOf(f), slotOf(g)),
		newScan: func() func(pgxRows) (R, error) {
			var (
				va A
				vb B
				vc C
				vd D
				vf F
				vg G
			)
			dest := []any{&va, &vb, &vc, &vd, &vf, &vg}
			return func(rows pgxRows) (R, error) {
				if err := rows.Scan(dest...); err != nil {
					var zero R
					return zero, err
				}
				return build(va, vb, vc, vd, vf, vg), nil
			}
		},
	}
}

// Project7 builds a seven-expression result shape.
func Project7[E, A, B, C, D, F, G, H, R any](a Selectable[E, A], b Selectable[E, B], c Selectable[E, C], d Selectable[E, D], f Selectable[E, F], g Selectable[E, G], h Selectable[E, H],
	build func(A, B, C, D, F, G, H) R,
) Projection[E, R] {
	return Projection[E, R]{
		items: items(a.selectItem(), b.selectItem(), c.selectItem(), d.selectItem(), f.selectItem(), g.selectItem(), h.selectItem()),
		shape: shapeOf(slotOf(a), slotOf(b), slotOf(c), slotOf(d), slotOf(f), slotOf(g), slotOf(h)),
		newScan: func() func(pgxRows) (R, error) {
			var (
				va A
				vb B
				vc C
				vd D
				vf F
				vg G
				vh H
			)
			dest := []any{&va, &vb, &vc, &vd, &vf, &vg, &vh}
			return func(rows pgxRows) (R, error) {
				if err := rows.Scan(dest...); err != nil {
					var zero R
					return zero, err
				}
				return build(va, vb, vc, vd, vf, vg, vh), nil
			}
		},
	}
}

// Project8 builds an eight-expression result shape.
// Eight is where this stops. A result wider than that is a query whose shape
// deserves a name of its own, and the entity path already reads whole rows.
func Project8[E, A, B, C, D, F, G, H, I, R any](a Selectable[E, A], b Selectable[E, B], c Selectable[E, C], d Selectable[E, D], f Selectable[E, F], g Selectable[E, G], h Selectable[E, H], i Selectable[E, I],
	build func(A, B, C, D, F, G, H, I) R,
) Projection[E, R] {
	return Projection[E, R]{
		items: items(a.selectItem(), b.selectItem(), c.selectItem(), d.selectItem(), f.selectItem(), g.selectItem(), h.selectItem(), i.selectItem()),
		shape: shapeOf(slotOf(a), slotOf(b), slotOf(c), slotOf(d), slotOf(f), slotOf(g), slotOf(h), slotOf(i)),
		newScan: func() func(pgxRows) (R, error) {
			var (
				va A
				vb B
				vc C
				vd D
				vf F
				vg G
				vh H
				vi I
			)
			dest := []any{&va, &vb, &vc, &vd, &vf, &vg, &vh, &vi}
			return func(rows pgxRows) (R, error) {
				if err := rows.Scan(dest...); err != nil {
					var zero R
					return zero, err
				}
				return build(va, vb, vc, vd, vf, vg, vh, vi), nil
			}
		},
	}
}
