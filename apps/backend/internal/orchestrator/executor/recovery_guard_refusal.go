package executor

import "errors"

// recoveryGuardRefusal is implemented by launch errors that report a refusal
// by the startup recovery guard (see orchestrator.SessionRecoveryGuardError).
type recoveryGuardRefusal interface {
	error
	RetryableRecoveryGuardRefusal() bool
}

// IsRetryableRecoveryGuardRefusal reports whether err is a retryable refusal
// by the startup recovery guard. Such a refusal must not change session state.
func IsRetryableRecoveryGuardRefusal(err error) bool {
	var refusal recoveryGuardRefusal
	return errors.As(err, &refusal) && refusal.RetryableRecoveryGuardRefusal()
}
