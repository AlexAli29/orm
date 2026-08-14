// Package domain exercises enum reconciliation in both directions.
package domain

// Status agrees with the status enum exactly.
type Status string

// The labels of status.
const (
	StatusPending Status = "pending"
	StatusActive  Status = "active"
	StatusBanned  Status = "banned"
)

// Color does not: it is missing green and invents blue.
type Color string

// The constants of Color.
const (
	ColorRed  Color = "red"
	ColorBlue Color = "blue"
)

//orm:table items
type Item struct {
	ID     int64
	Status Status
	Color  Color
	// An enum column needs a named type, not a bare string.
	Shade string
	Kind  string
}
