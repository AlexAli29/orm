package main_test

import (
	"strings"
	"testing"
)

// The M16 audit: the API forms the manifest has to tell apart.
//
// These are the changes that look like nothing in a diff of the source and are
// breaking anyway — a variadic that became a slice, a defined type that became
// an alias, a constant whose value moved. A compatibility gate that misses one
// of them is worse than no gate: it puts a green check over the break.

const forms = `package subject

// Level is a published enum.
type Level int

// The levels.
const (
	// Off is off.
	Off Level = 0
	// On is on.
	On Level = 1
)

// Retries is a published tunable.
const Retries = 3

// Prefix is a published string.
const Prefix = "orm_"

// Option configures a Store.
type Option func(*Store)

// Store is a thing.
type Store struct {
	// Name names it.
	Name string
	// Level is how loud.
	Level Level ` + "`json:\"level\"`" + `
	quiet bool
}

// Inner is embedded.
type Inner struct {
	// Depth is how deep.
	Depth int
}

// Outer embeds Inner.
type Outer struct {
	Inner
	// Width is how wide.
	Width int
}

// Alias names Store another way.
type Alias = Store

// Defined is its own type.
type Defined Store

// New makes one.
func New(name string, opts ...Option) *Store { return nil }

// Reader is implemented by callers.
type Reader interface {
	// Read reads.
	Read() ([]byte, error)
}
`

func TestForms_manifestDistinguishesThem(t *testing.T) {
	before := generate(t, tree(t, forms))

	for _, c := range []struct {
		what string
		from string
		to   string
	}{
		{"variadic becoming a slice", "opts ...Option) *Store", "opts []Option) *Store"},
		{"an alias becoming a defined type", "type Alias = Store", "type Alias Store"},
		{"a defined type becoming an alias", "type Defined Store", "type Defined = Store"},
		{"a constant value changing at the same type", "\tOn Level = 1", "\tOn Level = 2"},
		{"an untyped constant value changing", "const Retries = 3", "const Retries = 5"},
		{"a string constant value changing", `const Prefix = "orm_"`, `const Prefix = "orm2_"`},
		{"an untyped constant gaining a type", "const Retries = 3", "const Retries int = 3"},
		{"a struct tag changing", "`json:\"level\"`", "`json:\"lvl\"`"},
		{"an unexported field added", "\tquiet bool", "\tquiet bool\n\tloud bool"},
		{"an unexported field removed", "\tquiet bool\n", ""},
		{"an embedded field becoming named", "\tInner\n", "\tInner Inner\n"},
		{"an embedded struct losing a field", "\t// Depth is how deep.\n\tDepth int\n", ""},
	} {
		t.Run(c.what, func(t *testing.T) {
			mutated := strings.Replace(forms, c.from, c.to, 1)
			if mutated == forms {
				t.Fatalf("the mutation did not apply; the anchor %q is not in the subject", c.from)
			}
			after := generate(t, tree(t, mutated))
			if after == before {
				t.Errorf("%s produced an identical manifest, so CI would not have seen it", c.what)
			}
		})
	}
}
