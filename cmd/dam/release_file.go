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

const (
	filePollInterval    = 10 * time.Millisecond
	filePollMaxInterval = 250 * time.Millisecond
)

// releaseCoordinator serializes release-condition latches and fatal
// file-monitor observations. The mutex makes the OPEN/error boundary
// deterministic: a fatal result that has been reported before requestOpen is
// committed wins, while results after OPEN are ignored.
type releaseCoordinator struct {
	mu sync.Mutex

	release chan struct{}
	fatal   chan error
	files   chan struct{}

	initializing   bool
	pendingOpen    bool
	opened         bool
	completed      bool
	fatalErr       error
	filesStopped   bool
	groups         []releaseGroupState
	conditionIndex map[releaseConditionKey][]releaseMemberRef
	filePaths      map[string]*filePathState
}

func newReleaseCoordinator(initializing bool) *releaseCoordinator {
	return newReleaseCoordinatorWithGroups(initializing, nil)
}

type releaseGroupState struct {
	members   []releaseMemberState
	remaining int
}

type releaseMemberState struct {
	condition releaseCondition
	satisfied bool
}

type releaseConditionKey struct {
	kind   string
	source string
}

type releaseMemberRef struct {
	groupIndex  int
	memberIndex int
}

type filePathState struct {
	// remaining counts every matching file member, including duplicates and
	// occurrences in different OR groups.
	remaining int
}

func newReleaseCoordinatorWithGroups(initializing bool, groups []releaseGroup) *releaseCoordinator {
	groupStates := make([]releaseGroupState, len(groups))
	conditionIndex := make(map[releaseConditionKey][]releaseMemberRef)
	filePaths := make(map[string]*filePathState)
	for groupIndex, group := range groups {
		members := make([]releaseMemberState, len(group.members))
		for memberIndex, condition := range group.members {
			members[memberIndex] = releaseMemberState{condition: condition}
			ref := releaseMemberRef{groupIndex: groupIndex, memberIndex: memberIndex}
			key := releaseConditionKey{kind: condition.kind, source: condition.source}
			conditionIndex[key] = append(conditionIndex[key], ref)
			if condition.kind == "file" {
				state := filePaths[condition.source]
				if state == nil {
					state = &filePathState{}
					filePaths[condition.source] = state
				}
				state.remaining++
			}
		}
		groupStates[groupIndex] = releaseGroupState{members: members, remaining: len(members)}
	}
	return &releaseCoordinator{
		release:        make(chan struct{}),
		fatal:          make(chan error, 1),
		files:          make(chan struct{}),
		initializing:   initializing,
		groups:         groupStates,
		conditionIndex: conditionIndex,
		filePaths:      filePaths,
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
	c.reportFatalLocked(err)
}

func (c *releaseCoordinator) reportFatalLocked(err error) {
	if err == nil {
		return
	}
	if c.opened || c.completed || c.fatalErr != nil {
		return
	}
	c.fatalErr = err
	c.fatal <- err
	c.stopFilesLocked()
}

// satisfyCondition records one physical event for every matching member. It
// deliberately fans out instead of consuming an event per member: duplicate
// conditions and aliases describe the same latch, not a count of events.
func (c *releaseCoordinator) satisfyCondition(kind, source string) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.fatalErr != nil {
		return c.fatalErr
	}
	if c.opened || c.completed {
		return nil
	}

	if len(c.groups) == 0 {
		if c.initializing {
			c.pendingOpen = true
		} else {
			c.commitOpenLocked()
		}
		return nil
	}

	groupSatisfied := false
	key := releaseConditionKey{kind: kind, source: source}
	for _, ref := range c.conditionIndex[key] {
		group := &c.groups[ref.groupIndex]
		member := &group.members[ref.memberIndex]
		if member.satisfied {
			continue
		}
		member.satisfied = true
		group.remaining--
		if kind == "file" {
			if state := c.filePaths[source]; state != nil {
				state.remaining--
			}
		}
		if group.remaining == 0 {
			groupSatisfied = true
		}
	}
	if groupSatisfied {
		if c.initializing {
			c.pendingOpen = true
		} else {
			c.commitOpenLocked()
		}
	}
	return nil
}

func (c *releaseCoordinator) satisfySignal(signal string) error {
	switch signal {
	case "USR1":
		signal = "SIGUSR1"
	case "USR2":
		signal = "SIGUSR2"
	}
	return c.satisfyCondition("signal", signal)
}

func (c *releaseCoordinator) reportFileReady(path string) error {
	return c.satisfyCondition("file", path)
}

func (c *releaseCoordinator) filePathSatisfied(path string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.filePathSatisfiedLocked(path)
}

func (c *releaseCoordinator) filePathSatisfiedLocked(path string) bool {
	state := c.filePaths[path]
	return state != nil && state.remaining == 0
}

// reportFileFatal ignores an error from a path whose file latch was already
// satisfied. A satisfied member no longer needs monitoring, so a later probe
// result for that path cannot turn the latched condition back into a failure.
func (c *releaseCoordinator) reportFileFatal(path string, err error) {
	if err == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.filePathSatisfiedLocked(path) {
		return
	}
	c.reportFatalLocked(err)
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

// beginFileProbe checks the stopped state under the same mutex used to stop
// monitoring. OPEN/EOF may stop monitoring after a true result; the probe may
// then run, but the transition does not wait for it and its result is ignored.
func (c *releaseCoordinator) beginFileProbe() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return !c.filesStopped
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
	wait        filePollWait
}

// filePollWait waits for one poll interval or for monitoring to stop. Tests
// inject this operation so backoff sequencing can be verified without
// depending on wall-clock scheduling.
type filePollWait func(path string, stop <-chan struct{}, delay time.Duration) bool

func nextFilePollInterval(interval time.Duration) time.Duration {
	if interval >= filePollMaxInterval {
		return filePollMaxInterval
	}
	next := interval * 2
	if next <= interval || next >= filePollMaxInterval {
		return filePollMaxInterval
	}
	return next
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
	for index, path := range monitor.paths {
		go func() {
			ready, err := monitor.probe(path)
			if errors.Is(err, fs.ErrNotExist) {
				ready, err = false, nil
			}
			results <- fileProbeResult{index: index, ready: ready, err: err}
		}()
	}

	orderedResults := collectOrderedInitialFileProbeResults(results, len(monitor.paths))
	firstFatal, anyReady := summarizeInitialFileProbeResults(orderedResults)
	if firstFatal != nil {
		// Report the ordered initial fatal before applying ready results so the
		// startup barrier wins even for callers using a non-initializing legacy
		// coordinator.
		coordinator.reportFatal(firstFatal)
	}
	if firstFatal == nil && len(coordinator.groups) > 0 {
		for _, result := range orderedResults {
			if result.ready {
				_ = coordinator.reportFileReady(monitor.paths[result.index])
			}
		}
	}
	if err := coordinator.finishInitial(); err != nil {
		return monitor, err
	}
	if firstFatal != nil {
		return monitor, firstFatal
	}
	if len(coordinator.groups) == 0 && anyReady {
		// Keep the legacy monitor path's fatal-before-open ordering. Grouped
		// coordinators record ready latches above and decide OPEN at the barrier.
		if err := coordinator.requestOpen(); err != nil {
			return monitor, err
		}
		return monitor, nil
	}

	for _, path := range monitor.paths {
		if monitoringStopped(coordinator.files) {
			break
		}
		if coordinator.filePathSatisfied(path) {
			continue
		}
		go monitor.watchPath(path)
	}
	return monitor, nil
}

type fileProbeResult struct {
	index int
	ready bool
	err   error
}

func collectInitialFileProbeResults(results <-chan fileProbeResult, count int) (firstFatal error, anyReady bool) {
	return summarizeInitialFileProbeResults(collectOrderedInitialFileProbeResults(results, count))
}

func collectOrderedInitialFileProbeResults(results <-chan fileProbeResult, count int) []fileProbeResult {
	orderedResults := make([]fileProbeResult, count)
	for range count {
		result := <-results
		orderedResults[result.index] = result
	}
	return orderedResults
}

func summarizeInitialFileProbeResults(orderedResults []fileProbeResult) (firstFatal error, anyReady bool) {
	for _, result := range orderedResults {
		if result.err != nil && firstFatal == nil {
			firstFatal = result.err
		}
		anyReady = anyReady || result.ready
	}
	return firstFatal, anyReady
}

func (m *fileMonitor) Close() {
	if m != nil && m.coordinator != nil {
		m.coordinator.stopFiles()
	}
}

func (m *fileMonitor) watchPath(path string) {
	if monitoringStopped(m.coordinator.files) {
		return
	}

	interval := m.interval
	for {
		if !m.waitForPoll(path, interval) {
			return
		}
		// Recheck the stopped state under the coordinator mutex before the
		// probe. OPEN/EOF may still race after this check; that probe's result
		// is then ignored.
		if !m.coordinator.beginFileProbe() {
			return
		}
		ready, err := m.probe(path)
		if errors.Is(err, fs.ErrNotExist) {
			ready, err = false, nil
		}
		if err != nil {
			m.coordinator.reportFileFatal(path, err)
			return
		}
		if ready {
			_ = m.coordinator.reportFileReady(path)
			return
		}
		interval = nextFilePollInterval(interval)
	}
}

func (m *fileMonitor) waitForPoll(path string, interval time.Duration) bool {
	if m.wait != nil {
		return m.wait(path, m.coordinator.files, interval)
	}
	timer := time.NewTimer(interval)
	defer timer.Stop()
	select {
	case <-m.coordinator.files:
		return false
	case <-timer.C:
		return true
	}
}

func monitoringStopped(stop <-chan struct{}) bool {
	select {
	case <-stop:
		return true
	default:
		return false
	}
}
