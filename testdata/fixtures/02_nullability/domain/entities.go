// Package domain exercises the two ways a Go type and a column can disagree
// about NULL.
package domain

import "database/sql"

//orm:table users
type User struct {
	ID int64

	// A nullable column with no Go value that means NULL.
	Nickname string
	// The same column shape, done correctly.
	Bio *string

	// A plain slice cannot tell SQL NULL from an empty array.
	Tags []string
	// The correct spelling of a nullable array.
	Labels *[]string

	// A map's nil is distinguishable, so it carries NULL.
	Meta map[string]any

	// A pointer over a column that can never be NULL.
	Email *string

	// The other null-capable wrapper.
	Legacy sql.Null[string]
}
