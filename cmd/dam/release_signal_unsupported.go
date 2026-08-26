//go:build !aix && !darwin && !dragonfly && !freebsd && !illumos && !linux && !netbsd && !openbsd && !solaris && !windows

package main

// This file makes unsupported targets buildable and rejects signal release
// configuration instead of silently accepting an option they cannot honor.

import "fmt"

type releaseMonitor struct {
	coordinator *releaseCoordinator
}

func newReleaseMonitor(configured []string, coordinators ...*releaseCoordinator) (*releaseMonitor, error) {
	if len(configured) != 0 {
		return nil, fmt.Errorf("signal release is not supported on this platform")
	}
	if len(coordinators) > 1 {
		return nil, fmt.Errorf("multiple release coordinators are not supported")
	}
	if len(coordinators) == 0 {
		return &releaseMonitor{}, nil
	}
	return &releaseMonitor{coordinator: coordinators[0]}, nil
}

func (monitor *releaseMonitor) Release() <-chan struct{} {
	if monitor.coordinator == nil {
		return nil
	}
	return monitor.coordinator.release
}

func (monitor *releaseMonitor) Failures() <-chan error {
	if monitor.coordinator == nil {
		return nil
	}
	return monitor.coordinator.fatal
}

func (monitor *releaseMonitor) Close() {
	if monitor.coordinator != nil {
		monitor.coordinator.stopFiles()
	}
}
