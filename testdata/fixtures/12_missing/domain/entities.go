// Package domain points at tables that do not exist and at one that has no
// primary key.
package domain

//orm:table logs
type Log struct {
	ID      int64
	Message string
	// No such column.
	Level string
}

//orm:table ghosts
type Ghost struct {
	ID int64
}

//orm:table other.things
type Thing struct {
	ID int64
}
