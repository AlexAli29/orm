// Package sales declares a relation whose target is in another package.
//
// It type-checks — Go is perfectly happy with a one-way reference — and the
// foreign key it implies is a real one. What cannot be done is generate the
// loader: that code goes into this package and needs catalog's descriptors,
// which are unexported in catalog. Hence E024.
//
// The relation in the other direction is not merely unsupported but
// unrepresentable: catalog referring back to sales would be an import cycle,
// which Go rejects before this tool is involved.
package sales

import (
	"github.com/AlexAli29/orm"
	"github.com/AlexAli29/orm/testdata/fixtures/22_cross_package/catalog"
)

//orm:table orders
type Order struct {
	ID        int64 `orm:"pk,identity"`
	ProductID int64

	Product orm.One[catalog.Product] `orm:"fk:product_id"`
}
