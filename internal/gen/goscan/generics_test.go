package goscan_test

import (
	"strings"
	"testing"

	"github.com/AlexAli29/orm/internal/gen/model"
)

// The scanner keeps a generic instantiation's type arguments.
//
// Named is the origin's name — Box, not Box[int32] — because that is what
// go/types reports for the object. Without the arguments beside it, every
// instantiation of one generic would be one type to the reconciler, and
// Range[int32] would be indistinguishable from Range[int64].
func TestScan_genericTypeArguments(t *testing.T) {
	res := scan(t, "generics")
	e := entity(t, res, "Thing")

	byName := map[string]model.GoType{}
	for _, f := range e.Fields {
		byName[f.Name] = f.Type
	}

	const box = "github.com/AlexAli29/orm/internal/gen/goscan/testdata/generics.Box"
	const pair = "github.com/AlexAli29/orm/internal/gen/goscan/testdata/generics.Pair"

	t.Run("one origin, different arguments", func(t *testing.T) {
		for _, tt := range []struct {
			field string
			arg   string
		}{
			{"Ints", "int32"},
			{"Longs", "int64"},
			{"Strs", "string"},
			{"Times", "time.Time"},
		} {
			got := byName[tt.field]
			if got.Named != box {
				t.Errorf("%s.Named = %q, want the generic origin", tt.field, got.Named)
			}
			if len(got.TypeArgs) != 1 {
				t.Fatalf("%s has %d type arguments", tt.field, len(got.TypeArgs))
			}
			if arg := got.TypeArgs[0]; arg.Src != tt.arg {
				t.Errorf("%s argument = %q, want %q", tt.field, arg.Src, tt.arg)
			}
		}
		// And the identities differ, which is the whole point.
		seen := map[string]string{}
		for _, f := range []string{"Ints", "Longs", "Strs", "Times"} {
			g := byName[f].Generic()
			if other, ok := seen[g]; ok {
				t.Errorf("%s and %s both normalise to %q", f, other, g)
			}
			seen[g] = f
		}
	})

	t.Run("nested instantiation", func(t *testing.T) {
		got := byName["Nested"]
		if len(got.TypeArgs) != 1 {
			t.Fatalf("Nested has %d type arguments", len(got.TypeArgs))
		}
		inner := got.TypeArgs[0]
		if inner.Named != box || len(inner.TypeArgs) != 1 || inner.TypeArgs[0].Src != "int32" {
			t.Errorf("the nested argument is %+v", inner)
		}
		if want := box + "[" + box + "[int32]]"; got.Generic() != want {
			t.Errorf("Generic() = %q, want %q", got.Generic(), want)
		}
	})

	t.Run("two arguments keep their order", func(t *testing.T) {
		got := byName["Two"]
		if got.Named != pair {
			t.Errorf("Two.Named = %q", got.Named)
		}
		if len(got.TypeArgs) != 2 {
			t.Fatalf("Two has %d type arguments", len(got.TypeArgs))
		}
		if got.TypeArgs[0].Src != "int32" || got.TypeArgs[1].Src != "string" {
			t.Errorf("Two arguments = %q, %q", got.TypeArgs[0].Src, got.TypeArgs[1].Src)
		}
	})

	t.Run("a pointer to an instantiation keeps both facts", func(t *testing.T) {
		got := byName["PtrInts"]
		if !got.Ptr {
			t.Error("PtrInts is not reported as a pointer")
		}
		if got.Named != box || len(got.TypeArgs) != 1 || got.TypeArgs[0].Src != "int32" {
			t.Errorf("PtrInts = %+v", got)
		}
	})

	// Since Go 1.23 an alias is its own go/types node rather than the type it
	// names, and this scanner asks for a *types.Named. So an alias records
	// neither a qualified name nor type arguments — which is exactly what it
	// did before generics were understood, and is why adding them changed
	// nothing for aliases. A defined type over the same instantiation is a
	// different matter: it is its own type, keeps its own name, and is not
	// itself generic.
	t.Run("aliases stay unnamed and defined types keep their own identity", func(t *testing.T) {
		alias := byName["Aliased"]
		if alias.Named != "" || len(alias.TypeArgs) != 0 {
			t.Errorf("the alias reports Named=%q with %d arguments; the scanner does not resolve aliases",
				alias.Named, len(alias.TypeArgs))
		}
		if alias.Src != "Alias" {
			t.Errorf("the alias is written as %q", alias.Src)
		}
		defined := byName["Distinct"]
		if defined.Named != "github.com/AlexAli29/orm/internal/gen/goscan/testdata/generics.Defined" {
			t.Errorf("the defined type is named %q", defined.Named)
		}
		if len(defined.TypeArgs) != 0 {
			t.Errorf("the defined type reports %d type arguments; it is not generic", len(defined.TypeArgs))
		}
	})

	t.Run("a non-generic type carries no arguments", func(t *testing.T) {
		plain := byName["Plain"]
		if len(plain.TypeArgs) != 0 {
			t.Errorf("a string reports %d type arguments", len(plain.TypeArgs))
		}
		if plain.Named != "" {
			t.Errorf("a string reports Named=%q", plain.Named)
		}
		if plain.Generic() != "string" {
			t.Errorf("Generic() = %q for a plain string", plain.Generic())
		}
	})
}

// Stage 23 and Stage 24: no two of the instantiations M12.2 relies on share a
// generic identity, and the identity changes when the type argument does.
//
// This is the property the whole design rests on. Range[int32] and Range[int64]
// are one origin — "…/orm.Range" — and if the type arguments were dropped
// anywhere between go/types and the mapping fingerprint, the two would become
// one type and int8range would silently map to int32.
func TestScan_m122InstantiationsDoNotCollide(t *testing.T) {
	res := scan(t, "generics")
	e := entity(t, res, "Shapes")
	byName := map[string]model.GoType{}
	for _, f := range e.Fields {
		byName[f.Name] = f.Type
	}

	seen := map[string]string{}
	for _, field := range []string{
		"RangeInt32", "RangeInt64", "RangeTime",
		"MultiInt32", "MultiInt64", "MultiTime",
		"NestedBoxed", "NestedBoxed64", "Interval",
	} {
		gt, ok := byName[field]
		if !ok {
			t.Fatalf("no field %s", field)
		}
		id := gt.Generic()
		if other, dup := seen[id]; dup {
			t.Errorf("%s and %s share the identity %q", field, other, id)
		}
		seen[id] = field
	}

	// A pointer changes nullability and nothing about identity: *Range[int32]
	// and Range[int32] are the same type behind different null capability, and
	// the mapping asks each question separately.
	if a, b := byName["RangeInt32"].Generic(), byName["NullRangeI32"].Generic(); a != b {
		t.Errorf("Range[int32] is %q and *Range[int32] is %q; a pointer changed the identity", a, b)
	}
	if !byName["NullRangeI32"].Ptr {
		t.Error("*Range[int32] did not record that it is a pointer")
	}

	// The identity really does carry the argument rather than merely differing.
	if id := byName["RangeInt32"].Generic(); !strings.HasSuffix(id, "[int32]") {
		t.Errorf("Range[int32] renders as %q", id)
	}
	if id := byName["MultiTime"].Generic(); !strings.HasSuffix(id, "[time.Time]") {
		t.Errorf("Multirange[time.Time] renders as %q", id)
	}
	// A non-generic type keeps the identity it always had.
	if id := byName["Interval"].Generic(); id != "github.com/AlexAli29/orm.Interval" {
		t.Errorf("Interval renders as %q", id)
	}
}

// Audit item 4: type arguments are a sequence, not a set.
//
// Nothing may sort them. Pair[int32,string] and Pair[string,int32] are two
// types, and a normalisation that ordered the arguments — to make comparison
// cheaper, say — would make them one.
func TestScan_typeArgumentOrderIsSignificant(t *testing.T) {
	res := scan(t, "generics")
	e := entity(t, res, "Thing")
	byName := map[string]model.GoType{}
	for _, f := range e.Fields {
		byName[f.Name] = f.Type
	}

	a, b := byName["Two"], byName["TwoSwapped"]
	if a.Named != b.Named {
		t.Fatalf("the two fields are not instantiations of one origin: %s vs %s", a.Named, b.Named)
	}
	if a.Generic() == b.Generic() {
		t.Fatalf("Pair[int32,string] and Pair[string,int32] share the identity %q", a.Generic())
	}
	if len(a.TypeArgs) != 2 || len(b.TypeArgs) != 2 {
		t.Fatalf("arguments = %d / %d, want two each", len(a.TypeArgs), len(b.TypeArgs))
	}
	// Declaration order, not sorted order.
	if a.TypeArgs[0].Src != "int32" || a.TypeArgs[1].Src != "string" {
		t.Errorf("Pair[int32,string] records %s, %s", a.TypeArgs[0].Src, a.TypeArgs[1].Src)
	}
	if b.TypeArgs[0].Src != "string" || b.TypeArgs[1].Src != "int32" {
		t.Errorf("Pair[string,int32] records %s, %s", b.TypeArgs[0].Src, b.TypeArgs[1].Src)
	}
}

// Audit item 5: the alias limitation is a limitation, not a hazard.
//
// An alias to a range must not be silently mapped as one, must not panic the
// scanner, and must produce the same answer every time. What it produces is
// nothing — no qualified name and no type arguments — which is what makes the
// reconciler report an unmapped type rather than guess a family.
func TestScan_aliasToARangeIsInertAndDeterministic(t *testing.T) {
	var first model.GoType
	for i := range 5 {
		res := scan(t, "generics")
		e := entity(t, res, "Thing")
		var got model.GoType
		for _, f := range e.Fields {
			if f.Name == "Aliased" {
				got = f.Type
			}
		}
		if got.Named != "" || len(got.TypeArgs) != 0 {
			t.Fatalf("the alias reported Named=%q with %d arguments; it must not be recognised",
				got.Named, len(got.TypeArgs))
		}
		if i == 0 {
			first = got
			continue
		}
		if got.Src != first.Src || got.Kind != first.Kind || got.Generic() != first.Generic() {
			t.Errorf("run %d gave %+v, run 0 gave %+v; the answer is not deterministic", i, got, first)
		}
	}
}
