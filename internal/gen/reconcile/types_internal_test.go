package reconcile

import "testing"

func TestColumnName(t *testing.T) {
	tests := []struct{ field, want string }{
		{"ID", "id"},
		{"AuthorID", "author_id"},
		{"CreatedAt", "created_at"},
		{"HTTPCode", "http_code"},
		{"Title", "title"},
		{"Email", "email"},
		{"OAuthToken", "o_auth_token"},
		{"Line1", "line1"},
		{"A", "a"},
		{"", ""},
	}
	for _, tt := range tests {
		if got := columnName(tt.field); got != tt.want {
			t.Errorf("columnName(%q) = %q, want %q", tt.field, got, tt.want)
		}
	}
}

func TestScalarShapes_everyMappedTypeHasADescription(t *testing.T) {
	for name, sh := range scalarShapes {
		if len(sh.kinds) == 0 {
			t.Errorf("%s accepts no Go kind", name)
		}
		if sh.goDesc == "" {
			t.Errorf("%s has no Go description to suggest", name)
		}
	}
}

func TestRequiresConfiguration_isNotAlsoBuiltIn(t *testing.T) {
	// numeric and uuid must never acquire a default mapping by accident: the
	// whole point is that the tool refuses to choose.
	for name := range requiresConfiguration {
		if _, ok := scalarShapes[name]; ok {
			t.Errorf("%s has both a built-in mapping and a configuration requirement", name)
		}
	}
}
