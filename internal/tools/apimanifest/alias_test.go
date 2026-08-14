package main_test

import (
	"path/filepath"
	"strings"
	"testing"
)

// The M16 audit: the alias attack.
//
// An exported alias to a type in an unexported package publishes that type's
// shape without publishing its package. The manifest handles the first hop —
// [reachable] records the alias target. What these tests ask is whether it
// handles the rest of the graph, because compatibility does not stop at one
// hop: if the alias target has an exported field whose type is another internal
// type, that second type's methods and fields are just as reachable from
// outside, and just as breaking to change.
//
// The subject below is a module with a two-level internal chain. Each mutation
// changes something an external caller can see, and the manifest has to notice.

// aliasTree writes a module whose public API reaches two levels into an
// unexported package.
func aliasTree(t *testing.T, deep string) string {
	t.Helper()
	dir := t.TempDir()
	write(t, filepath.Join(dir, "go.mod"), "module example.com/subject\n\ngo 1.24\n")
	write(t, filepath.Join(dir, "internal", "deep", "deep.go"), deep)
	write(t, filepath.Join(dir, "api.go"), aliasAPI)
	return dir
}

// aliasAPI is the public surface: every way an internal type can escape.
const aliasAPI = `package subject

import "example.com/subject/internal/deep"

// Handle is an alias to an internal struct.
type Handle = deep.Handle

// Failure is an alias to an internal error type.
type Failure = deep.Failure

// Sink is an alias to an internal interface.
type Sink = deep.Sink

// Boxed is an alias to an internal generic type.
type Boxed = deep.Box[int64]

// Carrier carries an internal type in an exported field.
type Carrier struct {
	// Handle is the thing carried.
	Handle *deep.Handle
}

// Make makes one.
func Make(name string) (*deep.Handle, error) { return nil, nil }

// Pack packs a value, constrained by an internal constraint.
func Pack[T deep.Bound](v T) deep.Box[T] { return deep.Box[T]{V: v} }
`

// aliasDeep is the internal package. Detail is reachable only through Handle,
// which is the hop the manifest has to follow.
const aliasDeep = `// Package deep is unexported and reachable anyway.
package deep

// Detail is reachable only through Handle's field and method.
type Detail struct {
	// Code is a code.
	Code string
	// Count is a count.
	Count int64
}

// Describe describes it.
func (d Detail) Describe() string { return d.Code }

// Handle is aliased publicly.
type Handle struct {
	// Detail is the second hop.
	Detail Detail
	// Name is a name.
	Name string
}

// Inspect returns the detail.
func (h *Handle) Inspect() Detail { return h.Detail }

// Failure is an aliased error type.
type Failure struct {
	// Reason is why.
	Reason Detail
}

// Error implements error.
func (f *Failure) Error() string { return f.Reason.Code }

// Sink is an aliased interface.
type Sink interface {
	// Accept accepts one.
	Accept(d Detail) error
}

// Bound constrains Box.
type Bound interface{ ~int64 | ~string }

// Box is an aliased generic type.
type Box[T Bound] struct {
	// V is the value.
	V T
}

// Get returns it.
func (b Box[T]) Get() T { return b.V }
`

// Every mutation here is a source-compatibility break for an external caller
// that holds a Handle, implements a Sink, or reads a Detail — all of which the
// public aliases let it do.
func TestAlias_manifestFollowsTheChain(t *testing.T) {
	before := generate(t, aliasTree(t, aliasDeep))

	for _, c := range []struct {
		what string
		from string
		to   string
	}{
		{"a field removed from a second-hop internal struct", "\t// Count is a count.\n\tCount int64\n", ""},
		{"a field widened on a second-hop internal struct", "\tCount int64", "\tCount int"},
		{"a method added to a second-hop internal struct", "// Describe describes it.", "// Reset resets it.\nfunc (d *Detail) Reset() {}\n\n// Describe describes it."},
		{"a method removed from a second-hop internal struct", "func (d Detail) Describe() string { return d.Code }", ""},
		{"a method added to an aliased internal interface", "\tAccept(d Detail) error\n", "\tAccept(d Detail) error\n\t// Close closes.\n\tClose() error\n"},
		{"a parameter changed on an aliased internal interface", "Accept(d Detail) error", "Accept(d *Detail) error"},
		{"a narrowed internal constraint on a public generic function", "type Bound interface{ ~int64 | ~string }", "type Bound interface{ ~int64 }"},
		{"an exported field on the alias target becoming unexported", "\t// Name is a name.\n\tName string", "\t// name is a name.\n\tname string"},
		{"a method added to the alias target", "// Inspect returns the detail.", "// Close closes.\nfunc (h *Handle) Close() error { return nil }\n\n// Inspect returns the detail."},
		{"a field type changed on an internal error type", "\tReason Detail", "\tReason *Detail"},
	} {
		t.Run(c.what, func(t *testing.T) {
			mutated := strings.Replace(aliasDeep, c.from, c.to, 1)
			if mutated == aliasDeep {
				t.Fatalf("the mutation did not apply; the anchor %q is not in the subject", c.from)
			}
			after := generate(t, aliasTree(t, mutated))
			if after == before {
				t.Errorf("%s produced an identical manifest, so CI would not have seen it", c.what)
			}
		})
	}
}
