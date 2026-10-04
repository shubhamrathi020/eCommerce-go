// Package apperr is the one error type services return on purpose. It carries the same `code` and `fields`
// the frontend's ApiException understands, so the HTTP layer can render it one-to-one (the Nest API's AppError).
package apperr

import "fmt"

// Code is one of the shared ApiErrorCode values.
type Code string

const (
	Validation   Code = "validation"
	Unauthorized Code = "unauthorized"
	Forbidden    Code = "forbidden"
	NotFound     Code = "not_found"
	Conflict     Code = "conflict"
	Network      Code = "network"
	Unknown      Code = "unknown"
)

var status = map[Code]int{
	Validation:   400,
	Unauthorized: 401,
	Forbidden:    403,
	NotFound:     404,
	Conflict:     409,
	Network:      503,
	Unknown:      500,
}

// Error is an intentional, user-facing error.
type Error struct {
	Code    Code
	Message string
	Fields  map[string]string
}

func (e *Error) Error() string { return fmt.Sprintf("%s: %s", e.Code, e.Message) }

// Status is the HTTP status for this error's code.
func (e *Error) Status() int { return status[e.Code] }

// New builds an error without field messages.
func New(code Code, message string) *Error { return &Error{Code: code, Message: message} }

// WithFields builds an error carrying field-level messages.
func WithFields(code Code, message string, fields map[string]string) *Error {
	return &Error{Code: code, Message: message, Fields: fields}
}

// AssertNoFieldErrors returns the standard "check the highlighted fields" validation error when fields has any entry.
func AssertNoFieldErrors(fields map[string]string) error {
	if len(fields) > 0 {
		return WithFields(Validation, "Please check the highlighted fields.", fields)
	}
	return nil
}
