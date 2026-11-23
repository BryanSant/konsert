//go:build !darwin && !linux && !windows

package worker

func defaultWakeBackend() wakeBackend { return unsupportedWakeBackend{} }
