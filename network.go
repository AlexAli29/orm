package orm

import (
	"net/netip"

	"github.com/AlexAli29/orm/internal/expr"
)

// Network addresses.
//
// PostgreSQL's inet and cidr are one thing: an address together with a prefix
// length. That is why both map to netip.Prefix rather than to a string — a
// string keeps the text and loses the structure every operator below is defined
// in terms of, and "is this address inside that network" is not a question
// about text.
//
// The difference between the two types is what PostgreSQL will store, not how
// it is read: cidr rejects bits set to the right of the prefix and inet does
// not. That is the server's rule, checked by the server, and this package does
// not restate it.
//
// The operators are PostgreSQL's own. They are written by this package, never
// by a caller, and both sides are bind parameters.

// ContainedBy builds a << b: the left address or network is inside the right
// network, and is not equal to it.
func ContainedBy[A, B any](a Optional[A, *netip.Prefix], b Optional[B, *netip.Prefix]) Predicate[Composed] {
	return netOp("<<", a, b)
}

// ContainedByOrEquals builds a <<= b.
func ContainedByOrEquals[A, B any](a Optional[A, *netip.Prefix], b Optional[B, *netip.Prefix]) Predicate[Composed] {
	return netOp("<<=", a, b)
}

// ContainsNetwork builds a >> b: the left network contains the right address or
// network, and is not equal to it.
func ContainsNetwork[A, B any](a Optional[A, *netip.Prefix], b Optional[B, *netip.Prefix]) Predicate[Composed] {
	return netOp(">>", a, b)
}

// ContainsNetworkOrEquals builds a >>= b.
func ContainsNetworkOrEquals[A, B any](a Optional[A, *netip.Prefix], b Optional[B, *netip.Prefix]) Predicate[Composed] {
	return netOp(">>=", a, b)
}

// NetworksOverlap builds a && b: either network contains the other.
func NetworksOverlap[A, B any](a Optional[A, *netip.Prefix], b Optional[B, *netip.Prefix]) Predicate[Composed] {
	return netOp("&&", a, b)
}

func netOp[A, B any](op string, a Optional[A, *netip.Prefix], b Optional[B, *netip.Prefix]) Predicate[Composed] {
	return Predicate[Composed]{node: expr.Infix{
		Op: op, Left: a.optItem().Node, Right: b.optItem().Node,
	}}
}

// Network functions.
//
// Only the four whose result type PostgreSQL states plainly. host and text
// return text, masklen returns integer, and network returns cidr — which reads
// back as a netip.Prefix like any other.

// Host is PostgreSQL's host(inet): the address without its prefix length.
func Host[E any](v Selectable[E, netip.Prefix]) Expression[string, *string] {
	return Expression[string, *string]{node: expr.Call{Func: "host", Args: []expr.Node{v.selectItem().Node}}}
}

// HostNull is [Host] over a nullable address, which stays nullable.
func HostNull[E any](v Selectable[E, *netip.Prefix]) Expression[*string, *string] {
	return Expression[*string, *string]{
		node:     expr.Call{Func: "host", Args: []expr.Node{v.selectItem().Node}},
		nullSafe: true,
	}
}

// MaskLen is PostgreSQL's masklen(inet): the prefix length, as an integer.
func MaskLen[E any](v Selectable[E, netip.Prefix]) Expression[int32, *int32] {
	return Expression[int32, *int32]{node: expr.Call{Func: "masklen", Args: []expr.Node{v.selectItem().Node}}}
}

// MaskLenNull is [MaskLen] over a nullable address.
func MaskLenNull[E any](v Selectable[E, *netip.Prefix]) Expression[*int32, *int32] {
	return Expression[*int32, *int32]{
		node:     expr.Call{Func: "masklen", Args: []expr.Node{v.selectItem().Node}},
		nullSafe: true,
	}
}

// Network is PostgreSQL's network(inet): the address with its host bits zeroed,
// which PostgreSQL returns as a cidr and this package reads as a netip.Prefix.
func Network[E any](v Selectable[E, netip.Prefix]) Expression[netip.Prefix, *netip.Prefix] {
	return Expression[netip.Prefix, *netip.Prefix]{
		node: expr.Call{Func: "network", Args: []expr.Node{v.selectItem().Node}},
	}
}

// NetworkNull is [Network] over a nullable address.
func NetworkNull[E any](v Selectable[E, *netip.Prefix]) Expression[*netip.Prefix, *netip.Prefix] {
	return Expression[*netip.Prefix, *netip.Prefix]{
		node:     expr.Call{Func: "network", Args: []expr.Node{v.selectItem().Node}},
		nullSafe: true,
	}
}
