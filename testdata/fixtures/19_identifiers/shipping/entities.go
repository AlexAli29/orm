// Package shipping declares another Address, generating into the same
// directory as billing's.
package shipping

//orm:table shipping_addresses
type Address struct {
	ID   int64
	Line string
}
