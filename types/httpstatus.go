package types

import "errors"

// HTTPStatusError is implemented by provider errors built from a terminal
// non-2xx response, so a caller can tell a request the provider rejected
// (4xx) from a transport or stream fault without importing an adapter.
type HTTPStatusError interface {
	error
	HTTPStatus() int
}

// HTTPStatus returns the status of the first HTTPStatusError in err's
// chain. ok is false when err carries none.
func HTTPStatus(err error) (status int, ok bool) {
	var se HTTPStatusError
	if errors.As(err, &se) {
		return se.HTTPStatus(), true
	}
	return 0, false
}

// MarkHTTPStatus wraps err so HTTPStatus reports status. The wrapper keeps
// err's message and unwraps to err. A nil err returns nil.
func MarkHTTPStatus(err error, status int) error {
	if err == nil {
		return nil
	}
	return &httpStatusError{err: err, status: status}
}

type httpStatusError struct {
	err    error
	status int
}

func (e *httpStatusError) Error() string   { return e.err.Error() }
func (e *httpStatusError) Unwrap() error   { return e.err }
func (e *httpStatusError) HTTPStatus() int { return e.status }
