//go:build !linux && !darwin

package worker

import "time"

// ttyLastInput: no terminal devices to read here (Windows has no ttys).
func ttyLastInput(uint64) (string, time.Time, bool) { return "", time.Time{}, false }
