//go:build windows

package main

// This file keeps the duration and version paths buildable on Windows while
// rejecting the Unix-only signal release option explicitly.

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
