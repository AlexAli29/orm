// Package domain maps the two types that have no default representation,
// overrides one column name, and selects a configured mapping per field.
package domain

import "github.com/AlexAli29/orm"

// Decimal stands in for a third-party arbitrary-precision decimal.
type Decimal string

// UUID stands in for a third-party uuid.
type UUID [16]byte

// Money is a struct on purpose: nothing about a text column would accept it,
// so a field of this type maps only when a type: tag names the configured
// mapping.
type Money struct {
	Units int64
	Nanos int32
}

//orm:table payments
type Payment struct {
	ID     int64
	Amount Decimal
	Ref    UUID
	Name   string `orm:"column:legacy_name"`
	Fee    Money  `orm:"type:money_text"`

	// The same Go type against the same column shape, without the tag.
	FeeUntagged Money

	// A numrange reaches its element through the same configured mapping a
	// numeric column does, so it maps here and nowhere else.
	Band  orm.Range[Decimal]
	Tiers orm.Multirange[Decimal]

	// And the instantiation still has to be the right one.
	WrongBand orm.Range[string]
}
