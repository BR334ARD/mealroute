// Package domain contains business concepts independent from HTTP transport.
package domain

// Error is an expected business error. The HTTP layer maps its code to a
// response status and the OpenAPI ProblemDetails payload.
type Error struct {
	Code    string
	Message string
}

func (e *Error) Error() string {
	return e.Message
}

func NewError(code, message string) *Error {
	return &Error{Code: code, Message: message}
}
