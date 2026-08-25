//go:build aix || darwin || dragonfly || freebsd || illumos || linux || netbsd || openbsd || solaris

package main

// This file wires the supported Unix signal to a one-way release event while
// continuing to consume later occurrences until the command exits.

import (
	"os"
	"os/signal"
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
	if len(configured) == 0 {
		return &releaseMonitor{}, nil
	}

	monitor := &releaseMonitor{
		release: make(chan struct{}),
		done:    make(chan struct{}),
		signals: make(chan os.Signal, 1),
	}
	signal.Notify(monitor.signals, syscall.SIGUSR1)
	go monitor.consumeSignals()
	return monitor, nil
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
