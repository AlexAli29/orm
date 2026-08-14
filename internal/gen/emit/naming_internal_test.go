package emit

import (
	"strings"
	"testing"
)

func TestExportedFromTable(t *testing.T) {
	tests := []struct{ table, want string }{
		{"users", "Users"},
		{"billing_addresses", "BillingAddresses"},
		{"a", "A"},
		{"user_2fa", "User2fa"},
		{"Users", "Users"},
		{"", ""},
	}
	for _, tt := range tests {
		if got := exportedFromTable(tt.table); got != tt.want {
			t.Errorf("exportedFromTable(%q) = %q, want %q", tt.table, got, tt.want)
		}
	}
}

func TestSourceVarName(t *testing.T) {
	tests := []struct{ table, want string }{
		{"users", "usersSource"},
		{"billing_addresses", "billingAddressesSource"},
	}
	for _, tt := range tests {
		if got := sourceVarName(tt.table); got != tt.want {
			t.Errorf("sourceVarName(%q) = %q, want %q", tt.table, got, tt.want)
		}
	}
}

func TestUnexport(t *testing.T) {
	tests := []struct{ in, want string }{
		{"User", "user"},
		{"Post", "post"},
		{"ID", "id"},
		{"HTTPServer", "httpServer"},
		{"A", "a"},
		{"already", "already"},
		{"", ""},
	}
	for _, tt := range tests {
		if got := unexport(tt.in); got != tt.want {
			t.Errorf("unexport(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestEntityDerivedNames(t *testing.T) {
	if got := tableTypeName("User"); got != "userTable" {
		t.Errorf("tableTypeName = %q", got)
	}
	if got := metaVarName("HTTPLog"); got != "httpLogMeta" {
		t.Errorf("metaVarName = %q", got)
	}
	if got := destFuncName("Post"); got != "postDest" {
		t.Errorf("destFuncName = %q", got)
	}
}

func TestValidateIdentifier(t *testing.T) {
	tests := []struct {
		name, ident, want string
	}{
		{name: "ordinary", ident: "Users"},
		{name: "unexported", ident: "usersSource"},
		{name: "empty", ident: "", want: "empty"},
		{name: "leading digit", ident: "2fa", want: "not a Go identifier"},
		{name: "punctuation", ident: "a.b", want: "not a Go identifier"},
		{name: "space", ident: "a b", want: "not a Go identifier"},
		// The suffixes the naming policy adds mean no generated name is ever a
		// bare keyword, but the guard is what keeps that true.
		{name: "keyword", ident: "range", want: "Go keyword"},
		{name: "another keyword", ident: "type", want: "Go keyword"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateIdentifier(tt.ident, "source", "kind")
			if tt.want == "" {
				if err != nil {
					t.Fatalf("validateIdentifier(%q): %v", tt.ident, err)
				}
				return
			}
			if err == nil {
				t.Fatalf("validateIdentifier(%q) succeeded, want an error", tt.ident)
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("error = %v, want it to contain %q", err, tt.want)
			}
		})
	}
}

func TestIsStdlib(t *testing.T) {
	for _, p := range []string{"time", "encoding/json", "database/sql"} {
		if !isStdlib(p) {
			t.Errorf("%s is in the standard library", p)
		}
	}
	for _, p := range []string{"github.com/AlexAli29/orm", "gopkg.in/yaml.v3", "example.com/x"} {
		if isStdlib(p) {
			t.Errorf("%s is not in the standard library", p)
		}
	}
}
