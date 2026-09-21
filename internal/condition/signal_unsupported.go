//go:build !aix && !darwin && !dragonfly && !freebsd && !illumos && !linux && !netbsd && !openbsd && !solaris && !windows

package condition

// This file makes unsupported targets buildable and rejects signal release
// configuration instead of silently accepting an option they cannot honor.

import "fmt"

type releaseMonitor struct {
	engine *Engine
}

func signalReleaseSupported() bool { return false }

func newReleaseMonitor(configured []string, engines ...*Engine) (*releaseMonitor, error) {
	if len(configured) != 0 {
		return nil, fmt.Errorf("signal release is not supported on this platform")
	}
	if len(engines) > 1 {
		return nil, fmt.Errorf("multiple release engines are not supported")
	}
	if len(engines) == 0 {
		return &releaseMonitor{}, nil
	}
	return &releaseMonitor{engine: engines[0]}, nil
}

func (monitor *releaseMonitor) Release() <-chan struct{} {
	if monitor.engine == nil {
		return nil
	}
	return monitor.engine.release
}

func (monitor *releaseMonitor) Failures() <-chan error {
	if monitor.engine == nil {
		return nil
	}
	return monitor.engine.fatal
}

func (monitor *releaseMonitor) Close() {
	if monitor.engine != nil {
		monitor.engine.stopFiles()
	}
}
