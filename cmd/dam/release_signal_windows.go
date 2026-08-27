//go:build windows

package main

// This file keeps the duration and version paths buildable on Windows while
// rejecting the Unix-only signal release option explicitly.

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
