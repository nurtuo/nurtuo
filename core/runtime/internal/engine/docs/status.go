// SPDX-License-Identifier: AGPL-3.0-only

package docs

import "fmt"

// Status is the per-document processing outcome.
type Status uint8

// Document statuses.
const (
	StatusOK Status = iota
	StatusError
	StatusDropped
)

func (s Status) String() string {
	switch s {
	case StatusOK:
		return "ok"
	case StatusError:
		return "error"
	case StatusDropped:
		return "dropped"
	}
	return fmt.Sprintf("status(%d)", uint8(s))
}

// Error is the detail attached to a failed document.
type Error struct {
	Step    string // optional step/stage name
	Code    string // optional machine-readable code
	Message string
	Cause   error
}

func (e *Error) Error() string {
	s := e.Message
	if e.Code != "" {
		s = e.Code + ": " + s
	}
	if e.Step != "" {
		s = e.Step + ": " + s
	}
	return s
}

// Unwrap returns the underlying cause.
func (e *Error) Unwrap() error { return e.Cause }
