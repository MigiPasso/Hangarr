package auth

import "time"

// LockoutThreshold is the number of consecutive failures of one client that
// locks that client out (the account stays open to the others).
const LockoutThreshold = 5

// AccountAlertEvery: a warning is logged at every such number of failures
// on an account since its last sign-in, whatever the clients.
const AccountAlertEvery = 100

var lockoutSteps = []time.Duration{time.Minute, 5 * time.Minute, 15 * time.Minute, 60 * time.Minute}

// LockDuration returns how long a client is locked out of an account after
// failedAttempts consecutive failures: nothing below the threshold, then 1,
// 5, 15 and 60 minutes for each further failure (60 minutes from then on).
func LockDuration(failedAttempts int) time.Duration {
	if failedAttempts < LockoutThreshold {
		return 0
	}
	i := min(failedAttempts-LockoutThreshold, len(lockoutSteps)-1)
	return lockoutSteps[i]
}
