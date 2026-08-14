// Package domain holds self-references, which are the case where counting
// foreign keys cannot establish direction.
package domain

import "github.com/AlexAli29/orm"

//orm:table employees
type Employee struct {
	ID        int64
	ManagerID *int64
	Name      string

	// One foreign key, read as the parent.
	Manager orm.One[Employee]
	// The same foreign key, read as the children.
	Reports orm.Many[Employee]
	// The same foreign key again, pinned to the remote side, which turns it
	// into a has-one and so demands that employees.manager_id be unique.
	Deputy orm.One[Employee] `orm:"side:remote"`
}

//orm:table nodes
type Node struct {
	ID       int64
	ParentID *int64
	OriginID *int64

	// Two candidates and no tag.
	Parent orm.One[Node]
	// Pinned.
	Origin orm.One[Node] `orm:"fk:nodes_origin_fkey"`
	// Pinned, and read the other way round by its cardinality.
	Children orm.Many[Node] `orm:"fk:nodes_parent_fkey"`
}
