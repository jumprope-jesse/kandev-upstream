package executor

import "errors"

// recoveryGuardRefusal is implemented by launch errors that report a refusal
// by the startup recovery guard (see orchestrator.SessionRecoveryGuardError).
type recoveryGuardRefusal interface {
	error
	RecoveryGuardRefusal() bool
}

// IsRecoveryGuardRefusal reports whether err is a refusal
// by the startup recovery guard, retryable or not. Such a refusal must not change session state.
func IsRecoveryGuardRefusal(err error) bool {
	var refusal recoveryGuardRefusal
	return errors.As(err, &refusal) && refusal.RecoveryGuardRefusal()
}
