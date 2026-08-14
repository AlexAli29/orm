// Package inner exists only so that the boundary test has something to catch.
//
// It imports the ORM, which no package in core/ may do. Nothing in the
// application imports it, and cmd/server does not link it — it is a fixture,
// and it is here rather than in testdata/ because a fixture the toolchain does
// not list is a fixture `go list` cannot see.
package inner

import _ "github.com/AlexAli29/orm"
