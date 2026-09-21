package condition

// This file coordinates release sources and polls configured filesystem paths.

import (
	"fmt"
	"os"
	"slices"
	"time"
)

const (
	filePollInterval    = 10 * time.Millisecond
	filePollMaxInterval = 250 * time.Millisecond
)

// Condition is one typed release condition. Source is the canonical signal,
// duration, or datetime value, or the original file path.
type Condition struct {
	Kind     string
	Source   string
	Duration time.Duration
	Deadline time.Time
}

// Group is an AND group. Groups are alternatives (OR) in the order supplied.
type Group struct {
	Members []Condition
}

func newEngine(initializing bool) *Engine {
	return newEngineWithGroups(initializing, nil)
}

type groupState struct {
	members   []memberState
	remaining int
}

type memberState struct {
	condition Condition
	satisfied bool
}

type conditionKey struct {
	kind   string
	source string
}

type memberRef struct {
	groupIndex  int
	memberIndex int
}

type filePathState struct {
	// remaining counts every matching file member, including duplicates and
	// occurrences in different OR groups.
	remaining int
}

func newEngineWithGroups(initializing bool, groups []Group) *Engine {
	groupStates := make([]groupState, len(groups))
	conditionIndex := make(map[conditionKey][]memberRef)
	filePaths := make(map[string]*filePathState)
	for groupIndex, group := range groups {
		members := make([]memberState, len(group.Members))
		for memberIndex, condition := range group.Members {
			members[memberIndex] = memberState{condition: condition}
			ref := memberRef{groupIndex: groupIndex, memberIndex: memberIndex}
			key := conditionKeyFor(condition)
			conditionIndex[key] = append(conditionIndex[key], ref)
			if condition.Kind == "file" {
				state := filePaths[condition.Source]
				if state == nil {
					state = &filePathState{}
					filePaths[condition.Source] = state
				}
				state.remaining++
			}
		}
		groupStates[groupIndex] = groupState{members: members, remaining: len(members)}
	}
	return &Engine{
		release:        make(chan struct{}),
		fatal:          make(chan error, 1),
		files:          make(chan struct{}),
		initializing:   initializing,
		groups:         groupStates,
		conditionIndex: conditionIndex,
		filePaths:      filePaths,
	}
}

func (c *Engine) reportFatal(err error) {
	if err == nil {
		return
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	c.reportFatalLocked(err)
}

func (c *Engine) reportFatalLocked(err error) {
	if err == nil {
		return
	}
	if c.rootSatisfied || c.closed || c.fatalErr != nil {
		return
	}
	c.fatalErr = err
	c.fatal <- err
	c.stopFilesLocked()
}

// satisfyCondition records one physical event for every matching member. It
// deliberately fans out instead of consuming an event per member: duplicate
// conditions and aliases describe the same latch, not a count of events.
func (c *Engine) satisfyCondition(kind, source string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.fatalErr != nil {
		return c.fatalErr
	}
	if c.rootSatisfied || c.closed {
		return nil
	}

	if len(c.groups) == 0 {
		if c.initializing {
			c.pendingSatisfied = true
		} else {
			c.selectRootLocked()
		}
		return nil
	}

	groupSatisfied := false
	key := conditionKey{kind: kind, source: source}
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
			c.pendingSatisfied = true
		} else {
			timed := c.selectRootLocked()
			if timed != nil {
				timed.Close()
			}
		}
	}
	return nil
}

func (c *Engine) satisfySignal(signal string) error {
	switch signal {
	case "USR1":
		signal = "SIGUSR1"
	case "USR2":
		signal = "SIGUSR2"
	}
	return c.satisfyCondition("signal", signal)
}

func (c *Engine) reportFileReady(path string) error {
	return c.satisfyCondition("file", path)
}

func (c *Engine) filePathSatisfied(path string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.filePathSatisfiedLocked(path)
}

func (c *Engine) filePathSatisfiedLocked(path string) bool {
	state := c.filePaths[path]
	return state != nil && state.remaining == 0
}

// reportFileFatal remains available for engine-level failure reporting;
// file probes no longer call it because stat failures are retryable.
func (c *Engine) reportFileFatal(path string, err error) {
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

func (c *Engine) finishInitial() error {
	c.mu.Lock()
	c.initializing = false
	if c.fatalErr != nil {
		c.mu.Unlock()
		return c.fatalErr
	}
	if c.pendingSatisfied {
		timed := c.selectRootLocked()
		c.mu.Unlock()
		if timed != nil {
			timed.Close()
		}
		return nil
	}
	c.mu.Unlock()
	return nil
}

// selectRootLocked publishes the root condition result and stops only the
// file/timer monitors. Unix signal capture intentionally remains active until
// Engine.Close so later configured signals are consumed safely after OPEN.
func (c *Engine) selectRootLocked() *timedReleaseMonitor {
	if c.rootSatisfied || c.closed {
		return nil
	}
	c.rootSatisfied = true
	close(c.release)
	c.stopFilesLocked()
	return c.timed
}

func (c *Engine) stopFiles() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.stopFilesLocked()
}

func (c *Engine) stopFilesLocked() {
	if c.filesStopped {
		return
	}
	c.filesStopped = true
	close(c.files)
}

// beginFileProbe checks the stopped state under the same mutex used to stop
// monitoring. OPEN/EOF may stop monitoring after a true result; the probe may
// then run, but the transition does not wait for it and its result is ignored.
func (c *Engine) beginFileProbe() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return !c.filesStopped
}

func (c *Engine) fatalError() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.fatalErr
}

type fileProbe func(path string) (bool, error)

func probeFileRelease(path string) (bool, error) {
	info, err := os.Stat(path)
	if err != nil {
		return false, nil
	}
	return info.Mode().IsRegular(), nil
}

type fileMonitor struct {
	engine   *Engine
	paths    []string
	probe    fileProbe
	interval time.Duration
	wait     filePollWait
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

func newFileMonitor(paths []string, engine *Engine) (*fileMonitor, error) {
	return newFileMonitorWithProbe(paths, engine, probeFileRelease, filePollInterval)
}

func newFileMonitorWithProbe(paths []string, engine *Engine, probe fileProbe, interval time.Duration) (*fileMonitor, error) {
	if engine == nil {
		return nil, fmt.Errorf("file monitor requires a release engine")
	}
	if probe == nil {
		return nil, fmt.Errorf("file monitor requires a file probe")
	}
	if interval <= 0 {
		interval = time.Nanosecond
	}
	monitor := &fileMonitor{
		engine:   engine,
		paths:    slices.Clone(paths),
		probe:    probe,
		interval: interval,
	}
	if len(monitor.paths) == 0 {
		return monitor, nil
	}

	results := make(chan fileProbeResult, len(monitor.paths))
	for index, path := range monitor.paths {
		go func() {
			ready, err := monitor.probe(path)
			if err != nil {
				ready = false
			}
			results <- fileProbeResult{index: index, ready: ready, err: err}
		}()
	}

	orderedResults := collectOrderedInitialFileProbeResults(results, len(monitor.paths))
	_, anyReady := summarizeInitialFileProbeResults(orderedResults)
	if len(engine.groups) > 0 {
		for _, result := range orderedResults {
			if result.err == nil && result.ready {
				_ = engine.reportFileReady(monitor.paths[result.index])
			}
		}
	}
	if err := engine.finishInitial(); err != nil {
		return monitor, err
	}
	if len(engine.groups) == 0 && anyReady {
		engine.selectRoot()
		return monitor, nil
	}

	for _, path := range monitor.paths {
		if monitoringStopped(engine.files) {
			break
		}
		if engine.filePathSatisfied(path) {
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
		anyReady = anyReady || (result.err == nil && result.ready)
	}
	// The first return is retained for callers that still inspect the old
	// result shape; file probe errors are retryable and never become fatal.
	return nil, anyReady
}

func (m *fileMonitor) Close() {
	if m != nil && m.engine != nil {
		m.engine.stopFiles()
	}
}

func (m *fileMonitor) watchPath(path string) {
	if monitoringStopped(m.engine.files) {
		return
	}

	interval := m.interval
	for {
		if !m.waitForPoll(path, interval) {
			return
		}
		// Recheck the stopped state under the engine mutex before the
		// probe. OPEN/EOF may still race after this check; that probe's result
		// is then ignored.
		if !m.engine.beginFileProbe() {
			return
		}
		ready, err := m.probe(path)
		if err != nil {
			ready = false
		}
		if ready {
			_ = m.engine.reportFileReady(path)
			return
		}
		interval = nextFilePollInterval(interval)
	}
}

func (m *fileMonitor) waitForPoll(path string, interval time.Duration) bool {
	if m.wait != nil {
		return m.wait(path, m.engine.files, interval)
	}
	timer := time.NewTimer(interval)
	defer timer.Stop()
	select {
	case <-m.engine.files:
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
