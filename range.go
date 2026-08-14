package orm

import (
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5/pgtype"
)

// PostgreSQL ranges.
//
// A range is not a pair of endpoints. PostgreSQL's model has more states than
// that, and every one of them is reachable from SQL:
//
//	[1,10)   a finite range, inclusive below and exclusive above
//	(,10)    no lower bound
//	[1,)     no upper bound
//	(,)      no bound at all
//	empty    a range containing nothing
//
// A struct with a Start and an End cannot say which of those it is, so
// [Range] carries a bound kind per side and treats empty as a state of the
// whole value. That is the same model pgx uses on the wire, which is why the
// two round-trip without translation.
//
// SQL NULL is deliberately not one of the states. The ORM already spells
// nullability in the Go type — a NOT NULL column is Range[T] and a nullable one
// is *Range[T] — and giving Range its own Valid flag would be a second, quieter
// way to say the same thing, with nothing to stop the two disagreeing. It is
// also why this is a wrapper around pgx's protocol rather than an alias for
// pgtype.Range[T]: that type's Valid field would make the zero Range[T] mean
// SQL NULL, so a struct literal for a NOT NULL column would fail at the
// database rather than in the compiler.
//
// The zero value is the empty range. Of the two candidates it is the safe one:
// an accidental zero contains nothing rather than everything.
//
// # Canonicalisation
//
// PostgreSQL rewrites ranges over discrete types — int4range, int8range,
// daterange — into a canonical form on the way in. [1,10] is stored as [1,11),
// (1,10) as [2,10), and [1,1) as empty. Continuous types — numrange, tsrange,
// tstzrange — keep the bounds as written. A round-trip therefore returns the
// value PostgreSQL holds, which for a discrete range may not be the one that
// was sent. That is PostgreSQL's semantics and this package does not hide it.

// BoundKind is what a range's bound says about the values on its side.
//
// The zero value is [BoundEmpty], which makes the zero [Range] the empty range.
type BoundKind uint8

// The bound kinds.
const (
	// BoundEmpty marks a bound of the empty range. It appears on both sides or
	// on neither: emptiness is a property of the range, not of one end.
	BoundEmpty BoundKind = iota
	// BoundUnbounded means there is no bound on this side.
	BoundUnbounded
	// BoundInclusive means the bound value is in the range.
	BoundInclusive
	// BoundExclusive means the bound value is outside the range.
	BoundExclusive
)

// String renders the kind for diagnostics.
func (b BoundKind) String() string {
	switch b {
	case BoundUnbounded:
		return "unbounded"
	case BoundInclusive:
		return "inclusive"
	case BoundExclusive:
		return "exclusive"
	default:
		return "empty"
	}
}

// Bounded reports whether the bound has a value, which is true only for the
// inclusive and exclusive kinds.
func (b BoundKind) Bounded() bool { return b == BoundInclusive || b == BoundExclusive }

func (b BoundKind) pg() pgtype.BoundType {
	switch b {
	case BoundUnbounded:
		return pgtype.Unbounded
	case BoundInclusive:
		return pgtype.Inclusive
	case BoundExclusive:
		return pgtype.Exclusive
	default:
		return pgtype.Empty
	}
}

func boundKindOf(b pgtype.BoundType) BoundKind {
	switch b {
	case pgtype.Unbounded:
		return BoundUnbounded
	case pgtype.Inclusive:
		return BoundInclusive
	case pgtype.Exclusive:
		return BoundExclusive
	default:
		return BoundEmpty
	}
}

// Range is a PostgreSQL range over T.
//
// T is the range's element type, and it is what makes int4range and int8range
// different types in Go as well as in PostgreSQL: Range[int32] and Range[int64]
// do not convert to one another and no operator accepts both.
//
// Which PostgreSQL family a Range[T] maps to is decided by the column, never by
// T alone. daterange, tsrange and tstzrange all hold time.Time and remain three
// distinct types; the generator reads the family from the catalog in
// database-first mode, and in managed mode takes tstzrange unless a pgtype: tag
// names one of the other two.
//
// The fields are unexported because the bound kinds and the bound values have
// to agree — an inclusive bound with no value is not a range PostgreSQL can
// represent. Build one with a constructor and read it back with [Range.Bound],
// [Range.LowerBound] and [Range.UpperBound].
type Range[T any] struct {
	lo, hi         T
	loKind, hiKind BoundKind
}

// NewRange builds a range from its parts, and is the general form the named
// constructors are shorthands for.
//
// A bound kind that carries no value — [BoundUnbounded] or [BoundEmpty] —
// ignores the value given for that side. Passing [BoundEmpty] for either side
// produces the empty range, because PostgreSQL has no range that is empty at
// one end only.
func NewRange[T any](lo T, loKind BoundKind, hi T, hiKind BoundKind) Range[T] {
	if loKind == BoundEmpty || hiKind == BoundEmpty {
		return Range[T]{}
	}
	var zero T
	if !loKind.Bounded() {
		lo = zero
	}
	if !hiKind.Bounded() {
		hi = zero
	}
	return Range[T]{lo: lo, hi: hi, loKind: loKind, hiKind: hiKind}
}

// Closed builds [lo,hi]: both endpoints are in the range.
func Closed[T any](lo, hi T) Range[T] {
	return NewRange(lo, BoundInclusive, hi, BoundInclusive)
}

// Open builds (lo,hi): neither endpoint is in the range.
func Open[T any](lo, hi T) Range[T] {
	return NewRange(lo, BoundExclusive, hi, BoundExclusive)
}

// ClosedOpen builds [lo,hi), which is the form almost every interval over time
// or over integers wants: consecutive ranges meet without overlapping.
func ClosedOpen[T any](lo, hi T) Range[T] {
	return NewRange(lo, BoundInclusive, hi, BoundExclusive)
}

// OpenClosed builds (lo,hi].
func OpenClosed[T any](lo, hi T) Range[T] {
	return NewRange(lo, BoundExclusive, hi, BoundInclusive)
}

// RangeFrom builds [lo,): everything from lo upwards, with no upper bound.
func RangeFrom[T any](lo T) Range[T] {
	var zero T
	return NewRange(lo, BoundInclusive, zero, BoundUnbounded)
}

// RangeUntil builds (,hi): everything below hi, with no lower bound.
func RangeUntil[T any](hi T) Range[T] {
	var zero T
	return NewRange(zero, BoundUnbounded, hi, BoundExclusive)
}

// EmptyRange is the range containing nothing, and is the zero [Range].
//
// It is not SQL NULL and does not behave like it: an empty range is a value, it
// compares equal to other empty ranges, and isempty() reports true for it where
// it would report NULL for a NULL range.
func EmptyRange[T any]() Range[T] { return Range[T]{} }

// UnboundedRange is (,): the range containing every value of T.
func UnboundedRange[T any]() Range[T] {
	var zero T
	return NewRange(zero, BoundUnbounded, zero, BoundUnbounded)
}

// IsEmpty reports whether the range contains nothing.
func (r Range[T]) IsEmpty() bool { return r.loKind == BoundEmpty }

// LowerBound returns the lower bound's value and kind. The value is the zero T
// when the kind carries none.
func (r Range[T]) LowerBound() (T, BoundKind) { return r.lo, r.loKind }

// UpperBound returns the upper bound's value and kind.
func (r Range[T]) UpperBound() (T, BoundKind) { return r.hi, r.hiKind }

// Bound returns both bounds, for the callers that want them together.
func (r Range[T]) Bound() (lo T, loKind BoundKind, hi T, hiKind BoundKind) {
	return r.lo, r.loKind, r.hi, r.hiKind
}

// String renders the range the way PostgreSQL writes one, so that a value in a
// test failure reads like the literal it came from.
func (r Range[T]) String() string {
	if r.IsEmpty() {
		return "empty"
	}
	var b strings.Builder
	if r.loKind == BoundInclusive {
		b.WriteByte('[')
	} else {
		b.WriteByte('(')
	}
	if r.loKind.Bounded() {
		fmt.Fprintf(&b, "%v", r.lo)
	}
	b.WriteByte(',')
	if r.hiKind.Bounded() {
		fmt.Fprintf(&b, "%v", r.hi)
	}
	if r.hiKind == BoundInclusive {
		b.WriteByte(']')
	} else {
		b.WriteByte(')')
	}
	return b.String()
}

// The six methods below are pgx's range protocol, which is how a Range reaches
// PostgreSQL in the binary format without this package writing or parsing range
// text. They are not the API to call: encoding is the driver's business, and
// nothing in a query builder should be reaching for a bound type.

// IsNull implements pgx's range protocol. A Range is never SQL NULL — a
// nullable range column is *Range[T], and pgx nils that before it gets here.
func (r Range[T]) IsNull() bool { return false }

// BoundTypes implements pgx's range protocol.
func (r Range[T]) BoundTypes() (lower, upper pgtype.BoundType) {
	return r.loKind.pg(), r.hiKind.pg()
}

// Bounds implements pgx's range protocol.
func (r Range[T]) Bounds() (lower, upper any) { return &r.lo, &r.hi }

// ScanNull implements pgx's range protocol.
//
// It refuses, because reaching it means a NULL arrived for a Range[T] rather
// than a *Range[T] — the column is nullable and the Go type says it is not.
// Silently producing an empty range would turn "no value" into "a value that
// contains nothing", which is a different fact.
func (r *Range[T]) ScanNull() error {
	return fmt.Errorf("cannot scan NULL into orm.Range[%T]: declare the field as *orm.Range[%T] for a nullable range column", r.lo, r.lo)
}

// SetBoundTypes implements pgx's range protocol.
func (r *Range[T]) SetBoundTypes(lower, upper pgtype.BoundType) error {
	r.loKind, r.hiKind = boundKindOf(lower), boundKindOf(upper)
	// PostgreSQL never sends a range that is empty at one end, and a bound with
	// no value keeps whatever the scan target held. Normalising both is what
	// makes two equal ranges compare equal as Go values.
	var zero T
	if r.loKind == BoundEmpty || r.hiKind == BoundEmpty {
		r.loKind, r.hiKind = BoundEmpty, BoundEmpty
	}
	if !r.loKind.Bounded() {
		r.lo = zero
	}
	if !r.hiKind.Bounded() {
		r.hi = zero
	}
	return nil
}

// ScanBounds implements pgx's range protocol.
func (r *Range[T]) ScanBounds() (lowerTarget, upperTarget any) { return &r.lo, &r.hi }

// Multirange is a PostgreSQL multirange over T: an ordered set of
// non-overlapping, non-adjacent, non-empty ranges.
//
// # Canonicalisation
//
// PostgreSQL owns the contents. On the way in it sorts the components, merges
// any that overlap or touch, and drops the empty ones, so what comes back is
// the canonical set rather than the slice that was sent:
//
//	{[1,5),[3,9)}   arrives as  {[1,9)}
//	{[1,5),[5,9)}   arrives as  {[1,9)}     int4 is discrete, so these are adjacent
//	{[10,20),[1,5)} arrives as  {[1,5),[10,20)}
//	{[1,5),empty}   arrives as  {[1,5)}
//
// So a round-trip preserves the set of values, not the layout of the slice.
// Comparing a multirange before and after a write means comparing it to the
// canonical form, and this package makes no promise it does not keep.
//
// It is a slice because that is what a multirange is, and because the
// components are already [Range] values carrying the full bound model. Nothing
// is gained by hiding a slice behind a type that would only offer the same
// indexing back.
type Multirange[T any] []Range[T]

// IsNull implements pgx's multirange protocol.
//
// A nil Multirange is SQL NULL and an empty non-nil one is the empty multirange
// — the same distinction Go already draws, and the one PostgreSQL draws between
// NULL and '{}'. A nullable multirange column is still *Multirange[T], so that
// the two ways of writing NULL never both apply to the same field.
func (m Multirange[T]) IsNull() bool { return m == nil }

// Len implements pgx's multirange protocol.
func (m Multirange[T]) Len() int { return len(m) }

// Index implements pgx's multirange protocol.
func (m Multirange[T]) Index(i int) any { return m[i] }

// IndexType implements pgx's multirange protocol.
//
// pgx calls it while planning an encode, before it has looked at the value, so
// it must answer for a nil receiver as readily as for a full one — which it can,
// because the question is about the type rather than the contents.
func (m Multirange[T]) IndexType() any { return Range[T]{} }

// ScanNull implements pgx's multirange protocol.
func (m *Multirange[T]) ScanNull() error { *m = nil; return nil }

// SetLen implements pgx's multirange protocol.
func (m *Multirange[T]) SetLen(n int) error { *m = make(Multirange[T], n); return nil }

// ScanIndex implements pgx's multirange protocol.
func (m Multirange[T]) ScanIndex(i int) any { return &m[i] }

// ScanIndexType implements pgx's multirange protocol.
//
// The receiver is a pointer and is deliberately never read. pgx plans a scan
// against a typed nil — scanning a nullable multirange column reaches this with
// a nil *Multirange[T], because the destination is a **Multirange[T] whose
// pointer is not yet allocated — and a value receiver would be dereferenced by
// the wrapper Go generates for it and panic before the body ran.
func (m *Multirange[T]) ScanIndexType() any { return &Range[T]{} }

// clone copies the components, so that a value taken into a statement stops
// being a window onto the caller's slice.
//
// A Multirange is a slice, and a slice handed to a builder shares its backing
// array with whoever handed it over. Without this, writing to that array after
// building a predicate would change what the predicate means — silently, with
// the statement still compiling and still running. Every place a Multirange
// becomes an argument copies it first.
//
// A nil multirange clones to nil, because nil is SQL NULL and not an empty
// multirange. The components are [Range] values with no interior pointers of
// their own, so one level is the whole of it.
func (m Multirange[T]) clone() Multirange[T] {
	if m == nil {
		return nil
	}
	out := make(Multirange[T], len(m))
	copy(out, m)
	return out
}

// String renders the multirange the way PostgreSQL writes one.
func (m Multirange[T]) String() string {
	parts := make([]string, 0, len(m))
	for _, r := range m {
		parts = append(parts, r.String())
	}
	return "{" + strings.Join(parts, ",") + "}"
}
