// Package domain walks the PostgreSQL type space: what maps, what needs
// configuring, and what v1 refuses to guess at.
package domain

import (
	"encoding/json"
	"time"

	"github.com/AlexAli29/orm"
)

//orm:table things
type Thing struct {
	ID      int64
	Small   int16
	Medium  int32
	Big     int64
	Ratio   float32
	Precise float64
	Flag    bool
	Name    string
	Code    string
	Fixed   string
	CI      string
	// A domain reconciles through its base type.
	Mail    string
	Payload []byte
	Doc     map[string]any
	Legacy  json.RawMessage
	Words   []string
	Counts  []int64
	At      time.Time
	// timestamp without time zone: accepted, but the ambiguity is reported.
	Naive time.Time
	Day   time.Time
	// numeric and uuid have no default Go representation.
	Amount string
	Ref    string
	// interval maps to orm.Interval, which this is not.
	Span string
	// A range over the wrong element type: the container is right and the
	// instantiation is not, which is the mistake generics exist to catch.
	Quota orm.Range[int64]
	// numrange has no mapping until numeric has one.
	Price orm.Range[float64]
	// A multirange over the wrong element type.
	Slots orm.Multirange[int64]
	// A Go type alias. Since Go 1.23 an alias is its own go/types node, so the
	// scanner does not resolve it — and the reconciler must say so rather than
	// map it or crash.
	Window Period `orm:"column:window_"`
	Grid   [][]int32
}

// Period is an alias, which the generator does not recognise.
type Period = orm.Range[time.Time]
