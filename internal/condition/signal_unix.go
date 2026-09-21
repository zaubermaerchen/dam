//go:build aix || darwin || dragonfly || freebsd || illumos || linux || netbsd || openbsd || solaris

package condition

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
	release chan struct{}
	done    chan struct{}
	stop    sync.Once
	signals chan os.Signal
	engine  *Engine
}

func signalReleaseSupported() bool { return true }

func newReleaseMonitor(configured []string, engines ...*Engine) (*releaseMonitor, error) {
	if len(engines) > 1 {
		return nil, fmt.Errorf("multiple release engines are not supported")
	}
	var engine *Engine
	if len(engines) == 1 {
		engine = engines[0]
	}
	return newReleaseMonitorWithEngine(configured, engine)
}

func newReleaseMonitorWithEngine(configured []string, engine *Engine) (*releaseMonitor, error) {
	effectiveSignals, err := resolveReleaseSignals(configured)
	if err != nil {
		return nil, err
	}
	if engine == nil && len(effectiveSignals) == 0 {
		return &releaseMonitor{}, nil
	}
	if engine == nil {
		engine = newEngine(false)
	}

	monitor := &releaseMonitor{
		release: engine.release,
		done:    make(chan struct{}),
		signals: make(chan os.Signal, len(effectiveSignals)),
		engine:  engine,
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
		case received := <-monitor.signals:
			_ = monitor.engine.satisfySignal(canonicalReleaseSignal(received))
		case <-monitor.done:
			return
		}
	}
}

func canonicalReleaseSignal(received os.Signal) string {
	switch received {
	case syscall.SIGUSR1:
		return "SIGUSR1"
	case syscall.SIGUSR2:
		return "SIGUSR2"
	default:
		return ""
	}
}

func (monitor *releaseMonitor) Release() <-chan struct{} {
	return monitor.release
}

func (monitor *releaseMonitor) Failures() <-chan error {
	if monitor.engine == nil {
		return nil
	}
	return monitor.engine.fatal
}

func (monitor *releaseMonitor) Close() {
	monitor.stop.Do(func() {
		if monitor.done != nil {
			close(monitor.done)
		}
		if monitor.signals != nil {
			signal.Stop(monitor.signals)
		}
		if monitor.engine != nil {
			monitor.engine.stopFiles()
		}
	})
}
