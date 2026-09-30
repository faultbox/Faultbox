//go:build !linux

package connowner

const platformSupported = false

func processStart(string, int) (uint64, error)                { return 0, nil }
func ownsConnection(string, registration, tuple) (bool, bool) { return false, true }
