package orm

import (
	"reflect"
	"strings"
	"testing"
	"time"
)

// The result-shape tests.
//
// A shape is what a set operation compares, so a shape that quietly described
// the wrong columns would not fail here — it would accept a union that scans
// one branch's rows with the other branch's destinations. These tests are
// therefore about the shape agreeing with the thing it describes, not about the
// comparison being convenient.

type shapeEntity struct {
	ID       int64
	Name     string
	Age      int32
	Active   bool
	Nickname *string
	Seen     time.Time
}

var (
	shapeSrc  = NewSource("public", "shape_entity")
	shapeID   = NewOrdCol[shapeEntity, int64](shapeSrc, "id")
	shapeName = NewTextCol[shapeEntity](shapeSrc, "name")
	shapeAge  = NewOrdCol[shapeEntity, int32](shapeSrc, "age")
	shapeAct  = NewCol[shapeEntity, bool](shapeSrc, "active")
	shapeNick = NewNullTextCol[shapeEntity](shapeSrc, "nickname")
	shapeSeen = NewOrdCol[shapeEntity, time.Time](shapeSrc, "seen")

	shapeMeta = EntityMeta[shapeEntity]{
		Table:  TableID{Schema: "public", Name: "shape_entity"},
		Source: shapeSrc,
		Columns: []ColumnMeta{
			{Name: "id", Field: "ID", NotNull: true, Identity: true},
			{Name: "name", Field: "Name", NotNull: true},
			{Name: "age", Field: "Age", NotNull: true},
			{Name: "active", Field: "Active", NotNull: true},
			{Name: "nickname", Field: "Nickname"},
			{Name: "seen", Field: "Seen", NotNull: true},
		},
		Dest: func(e *shapeEntity, idx int) any {
			switch idx {
			case 0:
				return &e.ID
			case 1:
				return &e.Name
			case 2:
				return &e.Age
			case 3:
				return &e.Active
			case 4:
				return &e.Nickname
			case 5:
				return &e.Seen
			}
			return nil
		},
	}
)

// typeOf names a Go type the way a slot records it, so the expectations below
// read as types rather than as strings.
func typeOf[T any]() reflect.Type { return reflect.TypeOf((*T)(nil)).Elem() }

// projectionCase is one arity: the shape a ProjectN built, and the per-slot Go
// types that arity was given.
type projectionCase struct {
	arity int
	items []itemFacts
	shape resultShape
}

// itemFacts is what the select item — the other half of the pair — says about a
// column, so that the two descriptions can be compared instead of the shape
// being compared with itself.
type itemFacts struct {
	alias    string
	nullable bool
	goType   reflect.Type
}

// everyProjection builds one projection of every arity the package offers, each
// mixing nullable and non-nullable columns of several Go types, and reports the
// shape beside the facts the items carry.
func everyProjection() []projectionCase {
	type box struct{}
	var cases []projectionCase

	p1 := Project1(shapeID, func(int64) box { return box{} })
	cases = append(cases, projectionCase{1, []itemFacts{
		{"", false, typeOf[int64]()},
	}, p1.shape})

	p2 := Project2(shapeID, shapeNick, func(int64, *string) box { return box{} })
	cases = append(cases, projectionCase{2, []itemFacts{
		{"", false, typeOf[int64]()},
		{"", true, typeOf[*string]()},
	}, p2.shape})

	p3 := Project3(shapeID, shapeName.As("who"), shapeAge,
		func(int64, string, int32) box { return box{} })
	cases = append(cases, projectionCase{3, []itemFacts{
		{"", false, typeOf[int64]()},
		{"who", false, typeOf[string]()},
		{"", false, typeOf[int32]()},
	}, p3.shape})

	p4 := Project4(shapeID, shapeName, shapeNick, shapeAct,
		func(int64, string, *string, bool) box { return box{} })
	cases = append(cases, projectionCase{4, []itemFacts{
		{"", false, typeOf[int64]()},
		{"", false, typeOf[string]()},
		{"", true, typeOf[*string]()},
		{"", false, typeOf[bool]()},
	}, p4.shape})

	p5 := Project5(shapeID, shapeName, shapeAge, shapeAct, shapeSeen,
		func(int64, string, int32, bool, time.Time) box { return box{} })
	cases = append(cases, projectionCase{5, []itemFacts{
		{"", false, typeOf[int64]()},
		{"", false, typeOf[string]()},
		{"", false, typeOf[int32]()},
		{"", false, typeOf[bool]()},
		{"", false, typeOf[time.Time]()},
	}, p5.shape})

	p6 := Project6(shapeID, shapeName, shapeAge, shapeAct, shapeNick, shapeSeen,
		func(int64, string, int32, bool, *string, time.Time) box { return box{} })
	cases = append(cases, projectionCase{6, []itemFacts{
		{"", false, typeOf[int64]()},
		{"", false, typeOf[string]()},
		{"", false, typeOf[int32]()},
		{"", false, typeOf[bool]()},
		{"", true, typeOf[*string]()},
		{"", false, typeOf[time.Time]()},
	}, p6.shape})

	p7 := Project7(shapeID, shapeName, shapeAge, shapeAct, shapeNick, shapeSeen, shapeID.As("again"),
		func(int64, string, int32, bool, *string, time.Time, int64) box { return box{} })
	cases = append(cases, projectionCase{7, []itemFacts{
		{"", false, typeOf[int64]()},
		{"", false, typeOf[string]()},
		{"", false, typeOf[int32]()},
		{"", false, typeOf[bool]()},
		{"", true, typeOf[*string]()},
		{"", false, typeOf[time.Time]()},
		{"again", false, typeOf[int64]()},
	}, p7.shape})

	p8 := Project8(shapeID, shapeName, shapeAge, shapeAct, shapeNick, shapeSeen, shapeID.As("again"), shapeName.As("twice"),
		func(int64, string, int32, bool, *string, time.Time, int64, string) box { return box{} })
	cases = append(cases, projectionCase{8, []itemFacts{
		{"", false, typeOf[int64]()},
		{"", false, typeOf[string]()},
		{"", false, typeOf[int32]()},
		{"", false, typeOf[bool]()},
		{"", true, typeOf[*string]()},
		{"", false, typeOf[time.Time]()},
		{"again", false, typeOf[int64]()},
		{"twice", false, typeOf[string]()},
	}, p8.shape})

	return cases
}

func TestShape_everyProjectionDescribesExactlyTheColumnsItSelects(t *testing.T) {
	cases := everyProjection()
	// The arities are covered exhaustively, so an arity added later without a
	// shape is caught here rather than by the union that accepts it.
	if len(cases) != 8 {
		t.Fatalf("the suite covers %d arities and the package offers 8", len(cases))
	}
	for _, c := range cases {
		if len(c.items) != c.arity {
			t.Fatalf("the Project%d case describes %d expected columns", c.arity, len(c.items))
		}
		if c.shape.columns() != c.arity {
			t.Errorf("Project%d selects %d expressions and its shape has %d slots",
				c.arity, c.arity, c.shape.columns())
			continue
		}
		for i, want := range c.items {
			got := c.shape.slots[i]
			if got.goType != want.goType {
				t.Errorf("Project%d slot %d has Go type %s, want %s",
					c.arity, i+1, typeName(got.goType), typeName(want.goType))
			}
			if got.nullable != want.nullable {
				t.Errorf("Project%d slot %d is %s, want %s",
					c.arity, i+1, nullability(got.nullable), nullability(want.nullable))
			}
			// The alias travels with the slot even though the comparison
			// ignores it: a slot that lost it would be describing a different
			// column from the item beside it.
			if got.alias != want.alias {
				t.Errorf("Project%d slot %d is named %q, want %q", c.arity, i+1, got.alias, want.alias)
			}
		}
	}
}

// A shape and its select list are built from the same values, and this asserts
// they stayed in step: same count, same order, same nullability. A constructor
// that passed its arguments to items in one order and to shapeOf in another
// would produce a shape describing columns the statement does not select.
func TestShape_slotsAgreeWithTheSelectListPositionally(t *testing.T) {
	type box struct{}
	p := Project4(shapeNick, shapeID, shapeName.As("who"), shapeAge,
		func(*string, int64, string, int32) box { return box{} })

	if len(p.items) != len(p.shape.slots) {
		t.Fatalf("the projection selects %d expressions and describes %d slots",
			len(p.items), len(p.shape.slots))
	}
	for i := range p.items {
		if p.items[i].Nullable != p.shape.slots[i].nullable {
			t.Errorf("column %d: the select item is %s and the slot is %s",
				i+1, nullability(p.items[i].Nullable), nullability(p.shape.slots[i].nullable))
		}
		if p.items[i].Alias != p.shape.slots[i].alias {
			t.Errorf("column %d: the select item is named %q and the slot %q",
				i+1, p.items[i].Alias, p.shape.slots[i].alias)
		}
	}
	if p.shape.slots[0].goType != typeOf[*string]() || p.shape.slots[1].goType != typeOf[int64]() {
		t.Errorf("the slots are in a different order from the select list: %s, %s",
			typeName(p.shape.slots[0].goType), typeName(p.shape.slots[1].goType))
	}
}

// A projection whose shape does not match its select list is refused before a
// statement is built from it. Nothing constructs one — this is the guard that
// makes that true rather than assumed.
func TestShape_aProjectionDescribingTheWrongNumberOfColumnsIsRefused(t *testing.T) {
	type box struct{}
	p := Project2(shapeID, shapeName, func(int64, string) box { return box{} })
	p.shape = shapeOf(p.shape.slots[0])

	err := p.validate()
	if err == nil {
		t.Fatal("a projection describing 1 slot for 2 expressions validated")
	}
	if !strings.Contains(err.Error(), "1 result slot") || !strings.Contains(err.Error(), "2 expressions") {
		t.Errorf("validate reported %q, which does not say how the two counts differ", err)
	}
}

func TestShape_entityShapeComesFromTheGeneratedDescriptor(t *testing.T) {
	got, err := entityShape(&shapeMeta)
	if err != nil {
		t.Fatalf("entityShape: %v", err)
	}
	want := []resultSlot{
		{typeOf[int64](), false, "id"},
		{typeOf[string](), false, "name"},
		{typeOf[int32](), false, "age"},
		{typeOf[bool](), false, "active"},
		{typeOf[*string](), true, "nickname"},
		{typeOf[time.Time](), false, "seen"},
	}
	if got.columns() != len(want) {
		t.Fatalf("the entity has %d columns and its shape has %d slots", len(want), got.columns())
	}
	for i, w := range want {
		if got.slots[i] != w {
			t.Errorf("column %d = {%s %s %q}, want {%s %s %q}", i+1,
				typeName(got.slots[i].goType), nullability(got.slots[i].nullable), got.slots[i].alias,
				typeName(w.goType), nullability(w.nullable), w.alias)
		}
	}
}

// The entity shape is the descriptor's, not the struct's. Two entities with
// identical Go fields but different catalogs describe different results, and a
// shape read off reflect.TypeOf(E) could not tell them apart.
func TestShape_entityShapeIsNotDerivedFromTheStructAlone(t *testing.T) {
	relaxed := shapeMeta
	relaxed.Columns = append([]ColumnMeta(nil), shapeMeta.Columns...)
	relaxed.Columns[1].NotNull = false // the same Go field, a nullable column

	strict, err := entityShape(&shapeMeta)
	if err != nil {
		t.Fatalf("entityShape: %v", err)
	}
	loose, err := entityShape(&relaxed)
	if err != nil {
		t.Fatalf("entityShape: %v", err)
	}
	if err := compareResultShapes(strict, loose, 1, 2); err == nil {
		t.Fatal("two entities of one Go type but different nullability compared as the same shape, " +
			"so the shape is being read from the struct rather than from the descriptor")
	}
}

func TestShape_entityShapeRefusesADescriptorItCannotRead(t *testing.T) {
	noDest := shapeMeta
	noDest.Dest = nil

	empty := shapeMeta
	empty.Columns = nil

	short := shapeMeta
	short.Columns = append(append([]ColumnMeta(nil), shapeMeta.Columns...),
		ColumnMeta{Name: "extra", Field: "Extra", NotNull: true})

	cases := []struct {
		name string
		meta *EntityMeta[shapeEntity]
		want string
	}{
		{"no metadata", nil, "no metadata"},
		{"no columns", &empty, "no columns"},
		{"no destination function", &noDest, "no destination function"},
		{"a column with no destination", &short, "no destination for column 7"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := entityShape(c.meta)
			if err == nil {
				t.Fatalf("entityShape accepted %s", c.name)
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Errorf("entityShape reported %q, want it to mention %q", err, c.want)
			}
		})
	}
}

func TestCompare_acceptsTheSameShape(t *testing.T) {
	left := shapeOf(
		resultSlot{typeOf[int64](), false, "id"},
		resultSlot{typeOf[*string](), true, "nickname"},
	)
	right := shapeOf(
		resultSlot{typeOf[int64](), false, "id"},
		resultSlot{typeOf[*string](), true, "nickname"},
	)
	if err := compareResultShapes(left, right, 1, 2); err != nil {
		t.Errorf("two identical shapes were refused: %v", err)
	}
}

// Aliases are output names, not result identity: rows are scanned positionally
// and PostgreSQL names a compound's columns after its first branch. Requiring
// them to match would refuse unions that execute and read correctly.
func TestCompare_ignoresOutputNames(t *testing.T) {
	left := shapeOf(resultSlot{typeOf[int64](), false, "id"})
	right := shapeOf(resultSlot{typeOf[int64](), false, "identifier"})
	if err := compareResultShapes(left, right, 1, 2); err != nil {
		t.Errorf("branches naming one column differently were refused: %v", err)
	}
}

func TestCompare_refusesEveryMismatchWithoutWidening(t *testing.T) {
	cases := []struct {
		name        string
		left, right resultShape
		want        []string
	}{
		{
			name:  "a different number of columns",
			left:  shapeOf(resultSlot{typeOf[int64](), false, "id"}),
			right: shapeOf(resultSlot{typeOf[int64](), false, "id"}, resultSlot{typeOf[string](), false, "name"}),
			want:  []string{"branch 1 selects 1 column", "branch 2 selects 2"},
		},
		{
			// PostgreSQL would resolve int4 against int8 to int8. Doing that
			// here would mean choosing a Go destination the caller did not
			// write, so it is refused.
			name:  "int32 against int64",
			left:  shapeOf(resultSlot{typeOf[int64](), false, "n"}),
			right: shapeOf(resultSlot{typeOf[int32](), false, "n"}),
			want:  []string{"column 1", "int32", "int64"},
		},
		{
			name:  "a text type against a value type",
			left:  shapeOf(resultSlot{typeOf[string](), false, "k"}),
			right: shapeOf(resultSlot{typeOf[[16]byte](), false, "k"}),
			want:  []string{"column 1", "string", "[16]uint8"},
		},
		{
			name:  "T against *T",
			left:  shapeOf(resultSlot{typeOf[string](), false, "s"}),
			right: shapeOf(resultSlot{typeOf[*string](), false, "s"}),
			want:  []string{"column 1", "*string", "string"},
		},
		{
			name:  "nullable against not nullable",
			left:  shapeOf(resultSlot{typeOf[*string](), false, "s"}),
			right: shapeOf(resultSlot{typeOf[*string](), true, "s"}),
			want:  []string{"column 1", "nullable", "not nullable", "does not widen"},
		},
		{
			name:  "a mismatch after several matching columns",
			left:  shapeOf(resultSlot{typeOf[int64](), false, "a"}, resultSlot{typeOf[string](), false, "b"}, resultSlot{typeOf[bool](), false, "c"}),
			right: shapeOf(resultSlot{typeOf[int64](), false, "a"}, resultSlot{typeOf[string](), false, "b"}, resultSlot{typeOf[int64](), false, "c"}),
			want:  []string{"column 3"},
		},
		{
			name:  "a branch that describes nothing",
			left:  shapeOf(resultSlot{typeOf[int64](), false, "a"}),
			right: resultShape{},
			want:  []string{"branch 2 does not describe its result shape"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := compareResultShapes(c.left, c.right, 1, 2)
			if err == nil {
				t.Fatalf("%s was accepted", c.name)
			}
			for _, w := range c.want {
				if !strings.Contains(err.Error(), w) {
					t.Errorf("the diagnostic %q does not mention %q", err, w)
				}
			}
			// A reflect dump would be unstable between runs and unassertable.
			if strings.Contains(err.Error(), "0x") {
				t.Errorf("the diagnostic %q contains what looks like an address", err)
			}
		})
	}
}

// The comparison is symmetric in what it accepts and rejects: no ordering of
// the branches makes an incompatible pair compatible.
func TestCompare_isSymmetricInWhatItRefuses(t *testing.T) {
	a := shapeOf(resultSlot{typeOf[int64](), false, "n"})
	b := shapeOf(resultSlot{typeOf[int32](), false, "n"})
	if compareResultShapes(a, b, 1, 2) == nil || compareResultShapes(b, a, 1, 2) == nil {
		t.Fatal("an incompatible pair was accepted in one of the two orders")
	}
}

// An empty shape is known — a projection cannot select nothing, but a shape
// built from an empty slot list is still a description, and it must not be
// confused with the absence of one.
func TestShape_knownDistinguishesEmptyFromAbsent(t *testing.T) {
	if (resultShape{}).known() {
		t.Error("a shape that was never built reports itself as known")
	}
	if !shapeOf().known() {
		t.Error("a shape built with no slots reports itself as unknown")
	}
}
