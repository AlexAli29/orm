// Package domain leaves several columns unmapped, only one of which makes the
// entity unusable.
package domain

//orm:table invoices
type Invoice struct {
	ID     int64
	Number int64
	Net    int32
	Gross  int32
}
