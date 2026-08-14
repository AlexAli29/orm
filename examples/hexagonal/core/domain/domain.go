// Package domain is the centre of the hexagon: the types and the rules, and
// nothing else.
//
// It imports the standard library and nothing further. No ORM, no driver, no
// HTTP, no configuration. That is not a stylistic preference — it is the
// property that makes the rest of the arrangement worth its extra files,
// because a rule that cannot reach the database cannot be written to depend on
// one.
//
// The boundary is checked mechanically rather than by review: a test enumerates
// this package's imports and fails on anything outside the standard library.
package domain

import (
	"errors"
	"strings"
	"time"
)

// The failure categories. They are the vocabulary the outside translates from:
// an adapter maps a driver error onto one of these, and a transport maps one of
// these onto a status code. Neither knows the other's alphabet.
var (
	// ErrNotFound is a thing that is not there.
	ErrNotFound = errors.New("not found")
	// ErrConflict is a thing that is already there.
	ErrConflict = errors.New("conflict")
	// ErrInvalid is a request the rules reject.
	ErrInvalid = errors.New("invalid")
)

// User is a person who owns projects.
type User struct {
	ID        int64
	Email     string
	Name      string
	CreatedAt time.Time
}

// Validate states what a user must be, in the one place that decides it.
func (u User) Validate() error {
	switch {
	case strings.TrimSpace(u.Email) == "":
		return errors.Join(ErrInvalid, errors.New("a user needs an email address"))
	case !strings.Contains(u.Email, "@"):
		return errors.Join(ErrInvalid, errors.New("that is not an email address"))
	case strings.TrimSpace(u.Name) == "":
		return errors.Join(ErrInvalid, errors.New("a user needs a name"))
	case len(u.Name) > 200:
		return errors.Join(ErrInvalid, errors.New("that name is too long"))
	}
	return nil
}

// Project belongs to a user.
type Project struct {
	ID        int64
	OwnerID   int64
	Slug      string
	Name      string
	CreatedAt time.Time
}

// Validate states what a project must be.
//
// The slug rule is here rather than in a database constraint because it is a
// product decision — what a URL may contain — and the domain is where product
// decisions are readable. The database's unique index is a different thing: it
// enforces that no two projects share one, which is a fact about storage.
func (p Project) Validate() error {
	switch {
	case strings.TrimSpace(p.Slug) == "":
		return errors.Join(ErrInvalid, errors.New("a project needs a slug"))
	case !isSlug(p.Slug):
		return errors.Join(ErrInvalid, errors.New("a slug is lower-case letters, digits and hyphens"))
	case strings.TrimSpace(p.Name) == "":
		return errors.Join(ErrInvalid, errors.New("a project needs a name"))
	case p.OwnerID == 0:
		return errors.Join(ErrInvalid, errors.New("a project needs an owner"))
	}
	return nil
}

func isSlug(s string) bool {
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-':
		default:
			return false
		}
	}
	return true
}
