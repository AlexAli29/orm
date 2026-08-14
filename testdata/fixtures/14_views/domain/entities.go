// Package domain maps a view, which v1 reserves but does not support.
package domain

//orm:table users
type User struct {
	ID   int64
	Name string
}

//orm:view active_users
type ActiveUser struct {
	ID   int64
	Name string
}
