//go:build !aix && !darwin && !dragonfly && !freebsd && !illumos && !linux && !netbsd && !openbsd && !solaris && !windows

package main

// This file makes unsupported targets buildable and rejects signal release
// configuration instead of silently accepting an option they cannot honor.

import "fmt"

type releaseMonitor struct{}

func newReleaseMonitor(configured []string) (*releaseMonitor, error) {
	if len(configured) != 0 {
		return nil, fmt.Errorf("signal release is not supported on this platform")
	}
	return &releaseMonitor{}, nil
}

func (monitor *releaseMonitor) Release() <-chan struct{} {
	return nil
}

func (monitor *releaseMonitor) Close() {}
