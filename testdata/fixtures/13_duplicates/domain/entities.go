// Package domain maps two entities onto one table and two fields onto one
// column.
package domain

//orm:table users
type User struct {
	ID    int64
	Name  string
	Alias *string
}

//orm:table users
type Account struct {
	ID    int64
	Name  string
	Alias *string
}

//orm:table items
type Item struct {
	ID    int64
	Label string
	Alt   string `orm:"column:label"`
}
