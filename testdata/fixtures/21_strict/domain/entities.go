// Package domain is byte-for-byte the entity set of 20_warnings_only. Only the
// strict: policy differs, so the two fixtures together show that the severity
// of a configurable finding comes from the configuration and nothing else.
package domain

import "time"

//orm:table events
type Event struct {
	ID   int64
	Name string
	// timestamp without time zone: W015, suppressed here.
	Happened time.Time
	// events.note is left unmapped: W003, raised to an error here.
}
