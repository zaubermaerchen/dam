//go:build aix || darwin || dragonfly || freebsd || illumos || linux || netbsd || openbsd || solaris

package main

// This file wires the supported Unix signals to a one-way release event while
// continuing to consume later occurrences until the command exits.

import (
	"fmt"
	"os"
	"os/signal"
	"slices"
	"sync"
	"syscall"
)

type releaseMonitor struct {
	release     chan struct{}
	done        chan struct{}
	once        sync.Once
	stop        sync.Once
	signals     chan os.Signal
	coordinator *releaseCoordinator
}

func newReleaseMonitor(configured []string, coordinators ...*releaseCoordinator) (*releaseMonitor, error) {
	if len(coordinators) > 1 {
		return nil, fmt.Errorf("multiple release coordinators are not supported")
	}
	var coordinator *releaseCoordinator
	if len(coordinators) == 1 {
		coordinator = coordinators[0]
	}
	return newReleaseMonitorWithCoordinator(configured, coordinator)
}

func newReleaseMonitorWithCoordinator(configured []string, coordinator *releaseCoordinator) (*releaseMonitor, error) {
	effectiveSignals, err := resolveReleaseSignals(configured)
	if err != nil {
		return nil, err
	}
	if coordinator == nil && len(effectiveSignals) == 0 {
		return &releaseMonitor{}, nil
	}
	if coordinator == nil {
		coordinator = newReleaseCoordinator(false)
	}

	monitor := &releaseMonitor{
		release:     coordinator.release,
		done:        make(chan struct{}),
		signals:     make(chan os.Signal, len(effectiveSignals)),
		coordinator: coordinator,
	}
	if len(effectiveSignals) > 0 {
		signal.Notify(monitor.signals, effectiveSignals...)
		go monitor.consumeSignals()
	}
	return monitor, nil
}

func resolveReleaseSignals(configured []string) ([]os.Signal, error) {
	effective := make([]os.Signal, 0, len(configured))
	for _, canonical := range configured {
		var signal os.Signal
		switch canonical {
		case "SIGUSR1":
			signal = syscall.SIGUSR1
		case "SIGUSR2":
			signal = syscall.SIGUSR2
		default:
			return nil, fmt.Errorf("unknown configured release signal %q", canonical)
		}
		if !slices.Contains(effective, signal) {
			effective = append(effective, signal)
		}
	}
	return effective, nil
}

func (monitor *releaseMonitor) consumeSignals() {
	for {
		select {
		case <-monitor.signals:
			monitor.once.Do(func() { _ = monitor.coordinator.requestOpen() })
		case <-monitor.done:
			return
		}
	}
}

func (monitor *releaseMonitor) Release() <-chan struct{} {
	return monitor.release
}

func (monitor *releaseMonitor) Failures() <-chan error {
	if monitor.coordinator == nil {
		return nil
	}
	return monitor.coordinator.fatal
}

func (monitor *releaseMonitor) Close() {
	monitor.stop.Do(func() {
		if monitor.done != nil {
			close(monitor.done)
		}
		if monitor.signals != nil {
			signal.Stop(monitor.signals)
		}
		if monitor.coordinator != nil {
			monitor.coordinator.stopFiles()
		}
	})
}
