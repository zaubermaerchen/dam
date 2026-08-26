package main

// This file coordinates release sources and polls configured filesystem paths.

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"slices"
	"sync"
	"time"
)

const filePollInterval = 10 * time.Millisecond

// releaseCoordinator serializes release and fatal file-monitor observations.
// The mutex makes the OPEN/error boundary deterministic: a fatal result that
// has been reported before requestOpen is committed wins, while results after
// OPEN are ignored.
type releaseCoordinator struct {
	mu sync.Mutex

	release chan struct{}
	fatal   chan error
	files   chan struct{}

	initializing bool
	pendingOpen  bool
	opened       bool
	completed    bool
	fatalErr     error
	filesStopped bool
}

func newReleaseCoordinator(initializing bool) *releaseCoordinator {
	return &releaseCoordinator{
		release:      make(chan struct{}),
		fatal:        make(chan error, 1),
		files:        make(chan struct{}),
		initializing: initializing,
	}
}

func (c *releaseCoordinator) requestOpen() error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.fatalErr != nil {
		return c.fatalErr
	}
	if c.opened {
		return nil
	}
	if c.completed {
		return nil
	}
	if c.initializing {
		c.pendingOpen = true
		return nil
	}
	c.commitOpenLocked()
	return nil
}

func (c *releaseCoordinator) reportFatal(err error) {
	if err == nil {
		return
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	if c.opened || c.completed || c.fatalErr != nil {
		return
	}
	c.fatalErr = err
	c.fatal <- err
	c.stopFilesLocked()
}

func (c *releaseCoordinator) finishInitial() error {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.initializing = false
	if c.fatalErr != nil {
		return c.fatalErr
	}
	if c.pendingOpen {
		c.commitOpenLocked()
	}
	return nil
}

func (c *releaseCoordinator) completeEmpty() error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.fatalErr != nil {
		return c.fatalErr
	}
	if c.opened || c.completed {
		return nil
	}
	c.completed = true
	c.stopFilesLocked()
	return nil
}

func (c *releaseCoordinator) commitOpenLocked() {
	if c.opened {
		return
	}
	c.opened = true
	close(c.release)
	c.stopFilesLocked()
}

func (c *releaseCoordinator) stopFiles() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.stopFilesLocked()
}

func (c *releaseCoordinator) stopFilesLocked() {
	if c.filesStopped {
		return
	}
	c.filesStopped = true
	close(c.files)
}

func (c *releaseCoordinator) fatalError() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.fatalErr
}

type fileProbe func(path string) (bool, error)

func probeFileRelease(path string) (bool, error) {
	info, err := os.Stat(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return false, nil
		}
		return false, fmt.Errorf("cannot inspect release file %q: %w", path, err)
	}
	if !info.Mode().IsRegular() {
		return false, fmt.Errorf("release file %q is not a regular file", path)
	}
	return true, nil
}

type fileMonitor struct {
	coordinator *releaseCoordinator
	paths       []string
	probe       fileProbe
	interval    time.Duration
}

func newFileMonitor(paths []string, coordinator *releaseCoordinator) (*fileMonitor, error) {
	return newFileMonitorWithProbe(paths, coordinator, probeFileRelease, filePollInterval)
}

func newFileMonitorWithProbe(paths []string, coordinator *releaseCoordinator, probe fileProbe, interval time.Duration) (*fileMonitor, error) {
	if coordinator == nil {
		return nil, fmt.Errorf("file monitor requires a release coordinator")
	}
	if probe == nil {
		return nil, fmt.Errorf("file monitor requires a file probe")
	}
	if interval <= 0 {
		interval = time.Nanosecond
	}
	monitor := &fileMonitor{
		coordinator: coordinator,
		paths:       slices.Clone(paths),
		probe:       probe,
		interval:    interval,
	}
	if len(monitor.paths) == 0 {
		return monitor, nil
	}

	results := make(chan fileProbeResult, len(monitor.paths))
	for _, path := range monitor.paths {
		go func() {
			ready, err := monitor.probe(path)
			if errors.Is(err, fs.ErrNotExist) {
				ready, err = false, nil
			}
			results <- fileProbeResult{ready: ready, err: err}
		}()
	}

	var firstFatal error
	var anyReady bool
	for range monitor.paths {
		result := <-results
		if result.err != nil && firstFatal == nil {
			firstFatal = result.err
		}
		anyReady = anyReady || result.ready
	}
	if firstFatal != nil {
		coordinator.reportFatal(firstFatal)
	}
	if err := coordinator.finishInitial(); err != nil {
		return monitor, err
	}
	if firstFatal != nil {
		return monitor, firstFatal
	}
	if anyReady {
		if err := coordinator.requestOpen(); err != nil {
			return monitor, err
		}
		return monitor, nil
	}

	for _, path := range monitor.paths {
		go monitor.watchPath(path)
	}
	return monitor, nil
}

type fileProbeResult struct {
	ready bool
	err   error
}

func (m *fileMonitor) Close() {
	if m != nil && m.coordinator != nil {
		m.coordinator.stopFiles()
	}
}

func (m *fileMonitor) watchPath(path string) {
	ticker := time.NewTicker(m.interval)
	defer ticker.Stop()

	for {
		select {
		case <-m.coordinator.files:
			return
		case <-ticker.C:
			ready, err := m.probe(path)
			if errors.Is(err, fs.ErrNotExist) {
				continue
			}
			if err != nil {
				m.coordinator.reportFatal(err)
				return
			}
			if ready {
				_ = m.coordinator.requestOpen()
				return
			}
		}
	}
}
