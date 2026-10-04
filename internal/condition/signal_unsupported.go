//go:build !aix && !darwin && !dragonfly && !freebsd && !illumos && !linux && !netbsd && !openbsd && !solaris && !windows

package condition

// This file makes unsupported targets buildable and rejects signal release
// configuration instead of silently accepting an option they cannot honor.

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
