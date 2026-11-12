package hubingress

import "errors"

// RequestWasNotSent proves that the original credential-bearing request never
// reached the underlying transport. Anonymous identity probes may have run.
// A timeout or an arbitrary network error alone is not this proof.
func RequestWasNotSent(err error) bool {
	var sent *requestDispatchedError
	if errors.As(err, &sent) {
		return false
	}
	var before *requestNotSentError
	return errors.As(err, &before)
}

type requestNotSentError struct{ cause error }

func (e *requestNotSentError) Error() string { return e.cause.Error() }
func (e *requestNotSentError) Unwrap() error { return e.cause }

// Preserve ordinary error identity while preventing a nested transport's
// before-send marker from claiming this transport never crossed its boundary.
type requestDispatchedError struct{ cause error }

func (e *requestDispatchedError) Error() string { return e.cause.Error() }
func (e *requestDispatchedError) Unwrap() error { return e.cause }
