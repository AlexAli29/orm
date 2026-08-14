// Package viewdemo exercises views and materialized views as query sources.
//
// The entities and metadata here stand in for what the generator will produce.
// They are written by hand for the same reason the other demo packages were
// before their generators existed: the runtime contract has to be provable
// before there is a generator to prove it about, and a hand-written descriptor
// is the same descriptor a generated one will be.
package viewdemo

import (
	"time"

	"github.com/AlexAli29/orm"
)

// User is the base table.
type User struct {
	ID        int64
	Email     string
	Active    bool
	CreatedAt time.Time
	Settings  map[string]any
	LastIP    *string
}

// ActiveUser is an ordinary view over users.
type ActiveUser struct {
	ID        int64
	Email     string
	CreatedAt time.Time
	Settings  map[string]any
}

// UserOrder is the view whose price is nullable because of an outer join, even
// though the base column is NOT NULL.
type UserOrder struct {
	UserID int64
	Email  string
	// Price is a pointer because the LEFT JOIN inside the view can produce SQL
	// NULL for a user with no orders. The base column orders.price is NOT NULL,
	// and carrying that across would be a non-nullable field receiving NULL on
	// the first user who has never ordered.
	Price *string
}

// UserTotal is the materialized view.
type UserTotal struct {
	UserID int64
	Orders int64
	Total  *string
}

// The sources. One per relation, shared by every query built from it, which is
// what makes an alias distinguishable from the original.
var (
	usersSrc      = orm.NewSource("public", "users")
	activeSrc     = orm.NewSource("public", "active_users")
	userOrdersSrc = orm.NewSource("public", "user_orders")
	totalsSrc     = orm.NewSource("public", "user_totals")
)

// Users names the base table's columns.
var Users = struct {
	ID     orm.OrdCol[User, int64]
	Email  orm.TextCol[User]
	Active orm.Col[User, bool]
}{
	ID:     orm.NewOrdCol[User, int64](usersSrc, "id"),
	Email:  orm.NewTextCol[User](usersSrc, "email"),
	Active: orm.NewCol[User, bool](usersSrc, "active"),
}

// ActiveUsers names the view's columns. They are ordinary column descriptors:
// a view column is a column, and nothing below this line knows the difference.
var ActiveUsers = struct {
	ID        orm.OrdCol[ActiveUser, int64]
	Email     orm.TextCol[ActiveUser]
	CreatedAt orm.OrdCol[ActiveUser, time.Time]
}{
	ID:        orm.NewOrdCol[ActiveUser, int64](activeSrc, "id"),
	Email:     orm.NewTextCol[ActiveUser](activeSrc, "email"),
	CreatedAt: orm.NewOrdCol[ActiveUser, time.Time](activeSrc, "created_at"),
}

// UserOrders names the outer-join view's columns.
var UserOrders = struct {
	UserID orm.OrdCol[UserOrder, int64]
	Email  orm.TextCol[UserOrder]
}{
	UserID: orm.NewOrdCol[UserOrder, int64](userOrdersSrc, "user_id"),
	Email:  orm.NewTextCol[UserOrder](userOrdersSrc, "email"),
}

// UserTotals names the materialized view's columns.
var UserTotals = struct {
	UserID orm.OrdCol[UserTotal, int64]
	Orders orm.OrdCol[UserTotal, int64]
}{
	UserID: orm.NewOrdCol[UserTotal, int64](totalsSrc, "user_id"),
	Orders: orm.NewOrdCol[UserTotal, int64](totalsSrc, "orders"),
}

var userMeta = orm.EntityMeta[User]{
	Table:   orm.TableID{Schema: "public", Name: "users"},
	Source:  usersSrc,
	Columns: []orm.ColumnMeta{{Name: "id"}, {Name: "email"}, {Name: "active"}, {Name: "created_at"}, {Name: "settings"}, {Name: "last_ip"}},
	Dest: func(e *User, i int) any {
		return [...]any{&e.ID, &e.Email, &e.Active, &e.CreatedAt, &e.Settings, &e.LastIP}[i]
	},
	Value: func(e *User, i int) any {
		return [...]any{e.ID, e.Email, e.Active, e.CreatedAt, e.Settings, e.LastIP}[i]
	},
}

var activeMeta = orm.EntityMeta[ActiveUser]{
	Table:   orm.TableID{Schema: "public", Name: "active_users"},
	Source:  activeSrc,
	Columns: []orm.ColumnMeta{{Name: "id"}, {Name: "email"}, {Name: "created_at"}, {Name: "settings"}},
	Dest: func(e *ActiveUser, i int) any {
		return [...]any{&e.ID, &e.Email, &e.CreatedAt, &e.Settings}[i]
	},
	Value: func(e *ActiveUser, i int) any {
		return [...]any{e.ID, e.Email, e.CreatedAt, e.Settings}[i]
	},
}

var userOrderMeta = orm.EntityMeta[UserOrder]{
	Table:   orm.TableID{Schema: "public", Name: "user_orders"},
	Source:  userOrdersSrc,
	Columns: []orm.ColumnMeta{{Name: "user_id"}, {Name: "email"}, {Name: "price"}},
	Dest: func(e *UserOrder, i int) any {
		return [...]any{&e.UserID, &e.Email, &e.Price}[i]
	},
	Value: func(e *UserOrder, i int) any {
		return [...]any{e.UserID, e.Email, e.Price}[i]
	},
}

var totalMeta = orm.EntityMeta[UserTotal]{
	Table:   orm.TableID{Schema: "public", Name: "user_totals"},
	Source:  totalsSrc,
	Columns: []orm.ColumnMeta{{Name: "user_id"}, {Name: "orders"}, {Name: "total"}},
	Dest: func(e *UserTotal, i int) any {
		return [...]any{&e.UserID, &e.Orders, &e.Total}[i]
	},
	Value: func(e *UserTotal, i int) any {
		return [...]any{e.UserID, e.Orders, e.Total}[i]
	},
}

// DB is what the generator will produce: a table repository, two view
// repositories and a materialized-view repository, each with the capabilities
// its relation kind actually has.
type DB struct {
	Users       *orm.Repo[User]
	ActiveUsers *orm.ViewRepo[ActiveUser]
	UserOrders  *orm.ViewRepo[UserOrder]
	UserTotals  *orm.MaterializedViewRepo[UserTotal]
	// TotalsNoIndex is the same materialized view described without a
	// qualifying unique index, which is how a schema with no concurrent-refresh
	// candidate reaches the runtime.
	TotalsNoIndex *orm.MaterializedViewRepo[UserTotal]
}

// New binds the descriptors to an executor.
func New(ex orm.Executor) *DB {
	return &DB{
		Users:       orm.NewRepo(ex, &userMeta),
		ActiveUsers: orm.NewViewRepo(ex, &activeMeta),
		UserOrders:  orm.NewViewRepo(ex, &userOrderMeta),
		// The name of the unique index PostgreSQL needs for CONCURRENTLY. The
		// schema knows it; the runtime does not go looking.
		UserTotals:    orm.NewMaterializedViewRepo(ex, &totalMeta, "user_totals_user_id_key"),
		TotalsNoIndex: orm.NewMaterializedViewRepo(ex, &totalMeta, ""),
	}
}

// Sources, for tests that need to alias one.
func ActiveSource() *orm.Source { return activeSrc }
func TotalsSource() *orm.Source { return totalsSrc }
func UsersSource() *orm.Source  { return usersSrc }
