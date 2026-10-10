package types

import "errors"

// RetryableError is implemented by provider errors that report whether
// re-issuing the same model request may succeed. Provider adapters mark
// transient faults (overload, server errors, dropped or idle streams) so
// the agentic loop can retry a failed turn without importing a concrete
// adapter.
type RetryableError interface {
	error
	Retryable() bool
}

// IsRetryable reports whether the first RetryableError in err's chain
// reports true. An error with no RetryableError in its chain is not
// retryable.
func IsRetryable(err error) bool {
	var re RetryableError
	return errors.As(err, &re) && re.Retryable()
}

// MarkRetryable wraps err so IsRetryable reports true. The wrapper keeps
// err's message and unwraps to err. A nil err returns nil.
func MarkRetryable(err error) error {
	if err == nil {
		return nil
	}
	return &retryableError{err: err}
}

type retryableError struct{ err error }

func (e *retryableError) Error() string   { return e.err.Error() }
func (e *retryableError) Unwrap() error   { return e.err }
func (e *retryableError) Retryable() bool { return true }
