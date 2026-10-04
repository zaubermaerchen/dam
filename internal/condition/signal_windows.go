//go:build windows

package condition

// This file keeps the duration and version paths buildable on Windows while
// rejecting the Unix-only signal release option explicitly.

import "fmt"

type releaseMonitor struct{}

func signalReleaseSupported() bool { return false }

func newReleaseMonitor(configured []string, _ *Engine) (*releaseMonitor, error) {
	if len(configured) != 0 {
		return nil, fmt.Errorf("signal release is not supported on this platform")
	}
	return &releaseMonitor{}, nil
}

func (*releaseMonitor) Close() {}
