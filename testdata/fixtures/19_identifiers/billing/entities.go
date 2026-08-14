// Package billing declares an Address.
package billing

//orm:table billing_addresses
type Address struct {
	ID   int64
	Line string
}
