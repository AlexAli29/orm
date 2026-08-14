package goscan_test

import "testing"

// A type with no doc comment is ordinary Go, and a package that contains one
// must scan rather than crash.
//
// The scanner reads schema declarations from a type's comment group, and the
// group is nil when the type has none. Reading it anyway was a nil dereference
// that no fixture happened to reach, because every type in every other one is
// commented.
func TestScan_typesWithoutDocComments(t *testing.T) {
	res := scan(t, "undocumented")
	e := entity(t, res, "Widget")
	if len(e.Fields) != 2 {
		t.Errorf("Widget has %d fields, want 2", len(e.Fields))
	}
	if len(res.Decls) != 0 {
		t.Errorf("an undocumented type produced %d schema declarations", len(res.Decls))
	}
}
