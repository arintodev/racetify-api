// Package domain holds the plain data types shared across services and
// repositories, plus the sentinel errors repositories translate SQL-level
// failures (not found, unique violation) into so the service layer never
// has to know it is talking to Postgres.
package domain

import "errors"

var (
	ErrNotFound           = errors.New("domain: resource not found")
	ErrAlreadyExists      = errors.New("domain: resource already exists")
	ErrInvalidState       = errors.New("domain: invalid state transition")
	ErrTokenExpired       = errors.New("domain: token expired")
	ErrTokenConsumed      = errors.New("domain: token already used")
	ErrInvalidCredentials = errors.New("domain: invalid credentials")
	ErrAccountSuspended   = errors.New("domain: account suspended")
	ErrForbidden          = errors.New("domain: forbidden")
	ErrInvalidScope       = errors.New("domain: invalid or unauthorized scope")
)
