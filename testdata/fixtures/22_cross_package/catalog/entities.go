// Package catalog holds the target of the relation declared next door.
package catalog

//orm:table products
type Product struct {
	ID  int64  `orm:"pk,identity"`
	SKU string `orm:"unique"`
}
