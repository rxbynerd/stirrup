package types

import "errors"

// StopReasonContextWindowExceeded is the stop reason a provider reports when
// the response filled the model's context window before it finished.
const StopReasonContextWindowExceeded = "model_context_window_exceeded"

// ContextOverflowError is implemented by provider errors reporting that a
// request exceeded the model's context window. The loop recognises it
// through IsContextOverflow without importing a provider adapter.
type ContextOverflowError interface {
	error
	ContextOverflow() bool
}

type contextOverflowError struct {
	err error
}

func (e *contextOverflowError) Error() string         { return e.err.Error() }
func (e *contextOverflowError) Unwrap() error         { return e.err }
func (e *contextOverflowError) ContextOverflow() bool { return true }

// MarkContextOverflow wraps err so IsContextOverflow reports true for it,
// keeping its message and its wrapped chain. A nil err stays nil.
func MarkContextOverflow(err error) error {
	if err == nil {
		return nil
	}
	return &contextOverflowError{err: err}
}

// IsContextOverflow reports whether err, or an error it wraps, is a
// ContextOverflowError.
func IsContextOverflow(err error) bool {
	var overflow ContextOverflowError
	return errors.As(err, &overflow) && overflow.ContextOverflow()
}
