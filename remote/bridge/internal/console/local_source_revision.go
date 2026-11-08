package console

import (
	"crypto/subtle"
	"errors"

	"salcara/bridge/internal/config"
)

var errLocalSourceChanged = errors.New("账号来源已变化，请重新打开并确认")

// Legacy callers can omit the precondition. New account-card confirmation
// carries the exact process-bound revision displayed to the user; an empty
// supplied revision must never degrade into the legacy path.
func checkLocalSourceRevision(c config.Config, a config.LocalAccount, expected *string) error {
	if expected == nil {
		return nil
	}
	actual := c.AccountSourceRevision(a)
	if *expected == "" || actual == "" || subtle.ConstantTimeCompare([]byte(*expected), []byte(actual)) != 1 {
		return errLocalSourceChanged
	}
	return nil
}
