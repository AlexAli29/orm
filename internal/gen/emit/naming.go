package emit

import (
	"fmt"
	"go/token"
	"strings"
	"unicode"
)

// The naming policy.
//
// Public names come from the table, private ones from the entity. That split is
// not arbitrary: the descriptor a caller writes — Users.Email — reads as the
// table it queries, and the table is the thing PostgreSQL owns. The internal
// identifiers are per-entity because two entities that generate into one
// directory under one name is exactly what E014 reports, and deriving them from
// the entity keeps that check honest.
//
//	entity User, table public.users
//
//	Users            the table descriptor value          (from the table)
//	DB.Users         the repository field                (from the table)
//	usersSource      the table occurrence Users reads    (from the table)
//	userTable        the descriptor's type               (from the entity)
//	newUserTable     builds descriptors for one alias    (from the entity)
//	userMeta         the generated entity metadata       (from the entity)
//	userDest         the generated scanner               (from the entity)
//	userValue        the generated value accessor        (from the entity)
//
// Nothing is renamed to avoid a clash. A generated identifier that collides
// with one the author already wrote is reported, because silently becoming
// Users2 would leave the caller reading documentation that describes Users.
const (
	// DBTypeName is the generated per-package database handle.
	DBTypeName = "DB"
	// DBCtorName constructs it.
	DBCtorName = "New"
)

// exportedFromTable turns a table name into the exported identifier that
// addresses it: users becomes Users, billing_addresses becomes BillingAddresses.
func exportedFromTable(table string) string {
	return camel(table, true)
}

// sourceVarName is the unexported source shared by a table's descriptors.
func sourceVarName(table string) string {
	return camel(table, false) + "Source"
}

// tableTypeName is the descriptor struct's type.
func tableTypeName(entity string) string { return unexport(entity) + "Table" }

// tableCtorName builds the descriptors for one occurrence of the table.
func tableCtorName(entity string) string { return "new" + entity + "Table" }

// metaVarName is the generated entity metadata.
func metaVarName(entity string) string { return unexport(entity) + "Meta" }

// destFuncName is the generated scanner.
func destFuncName(entity string) string { return unexport(entity) + "Dest" }

// valueFuncName is the generated value accessor an insert reads through.
func valueFuncName(entity string) string { return unexport(entity) + "Value" }

// camel converts a snake_case SQL name to CamelCase. Runs of separators
// collapse, and a leading digit is kept so that the caller's validation can
// reject the result rather than this function inventing a prefix.
func camel(s string, exported bool) string {
	var b strings.Builder
	b.Grow(len(s))
	upper := exported
	for _, r := range s {
		if r == '_' || r == ' ' || r == '-' {
			upper = true
			continue
		}
		if upper {
			b.WriteRune(unicode.ToUpper(r))
			upper = false
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

// unexport lowers the leading run of capitals: User becomes user, ID becomes
// id, and HTTPServer becomes httpServer rather than hTTPServer or httpserver.
func unexport(s string) string {
	runes := []rune(s)
	n := 0
	for n < len(runes) && unicode.IsUpper(runes[n]) {
		n++
	}
	switch {
	case n == 0:
		return s
	case n == len(runes):
		// The whole name is capitals, so all of it is one word.
	default:
		// The last capital of a run starts the next word, so it stays.
		n = max(n-1, 1)
	}
	for i := range n {
		runes[i] = unicode.ToLower(runes[i])
	}
	return string(runes)
}

// validateIdentifier rejects a generated name that is not usable as one.
//
// A table called "2fa" or "select" produces something Go will not accept, and
// the honest response is to say so. Mangling it into a name the author never
// chose would leave them reading documentation about an identifier that does
// not exist.
func validateIdentifier(name, from, kind string) error {
	switch {
	case name == "":
		return fmt.Errorf("%s %q produces an empty identifier", kind, from)
	// The keyword check comes first because token.IsIdentifier reports false
	// for keywords, and "range is not an identifier" is a worse explanation
	// than "range is a keyword".
	case token.Lookup(name).IsKeyword():
		return fmt.Errorf("%s %q produces %q, which is a Go keyword", kind, from, name)
	case !token.IsIdentifier(name):
		return fmt.Errorf("%s %q produces %q, which is not a Go identifier", kind, from, name)
	default:
		return nil
	}
}
