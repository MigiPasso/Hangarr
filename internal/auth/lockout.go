package auth

import "time"

// LockoutThreshold is the number of consecutive failures that triggers the
// first lock.
const LockoutThreshold = 5

var lockoutSteps = []time.Duration{time.Minute, 5 * time.Minute, 15 * time.Minute, 60 * time.Minute}

// LockDuration returns how long the account is locked after failedAttempts
// consecutive failures: nothing below the threshold, then 1, 5, 15 and 60
// minutes for each further failure (60 minutes from then on).
func LockDuration(failedAttempts int) time.Duration {
	if failedAttempts < LockoutThreshold {
		return 0
	}
	i := min(failedAttempts-LockoutThreshold, len(lockoutSteps)-1)
	return lockoutSteps[i]
}
