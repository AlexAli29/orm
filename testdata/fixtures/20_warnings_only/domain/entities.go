// Package domain produces warnings and no errors, which is the case that makes
// --fail-on a threshold rather than a report-is-non-empty check.
package domain

import "time"

//orm:table events
type Event struct {
	ID   int64
	Name string
	// timestamp without time zone: W015.
	Happened time.Time
	// events.note is left unmapped: W003.
}
