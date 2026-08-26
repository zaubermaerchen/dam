//go:build aix || darwin || dragonfly || freebsd || illumos || linux || netbsd || openbsd || solaris

package main

// This file wires the supported Unix signal to a one-way release event while
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
	release chan struct{}
	done    chan struct{}
	once    sync.Once
	stop    sync.Once
	signals chan os.Signal
}

func newReleaseMonitor(configured []string) (*releaseMonitor, error) {
	effectiveSignals, err := resolveReleaseSignals(configured)
	if err != nil {
		return nil, err
	}
	if len(effectiveSignals) == 0 {
		return &releaseMonitor{}, nil
	}

	monitor := &releaseMonitor{
		release: make(chan struct{}),
		done:    make(chan struct{}),
		signals: make(chan os.Signal, len(effectiveSignals)),
	}
	signal.Notify(monitor.signals, effectiveSignals...)
	go monitor.consumeSignals()
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
			monitor.once.Do(func() { close(monitor.release) })
		case <-monitor.done:
			return
		}
	}
}

func (monitor *releaseMonitor) Release() <-chan struct{} {
	return monitor.release
}

func (monitor *releaseMonitor) Close() {
	monitor.stop.Do(func() {
		if monitor.done != nil {
			close(monitor.done)
		}
		if monitor.signals != nil {
			signal.Stop(monitor.signals)
		}
	})
}
