package condition

// Package condition owns typed release-condition composition and monitoring.
// It deliberately has no knowledge of argv, stream I/O, or public events.

import (
	"fmt"
	"slices"
	"sync"
	"time"
)

// Options supplies the small set of runtime dependencies needed by monitors.
// FileProbe and NewTimer are primarily useful for deterministic package tests;
// nil values select the production filesystem and time implementations.
type Options struct {
	Now              func() time.Time
	NewTimer         func(time.Duration) (<-chan time.Time, func())
	FileProbe        func(string) (bool, error)
	FilePollInterval time.Duration
}

// SignalSupported reports whether this build can monitor the configured Unix
// user signals.
func SignalSupported() bool { return signalReleaseSupported() }

// Engine evaluates OR alternatives of AND groups. Start performs startup
// monitoring and initial file probing; duration timers begin only after
// StartDurations is called by the stream runtime.
type Engine struct {
	mu sync.Mutex

	release chan struct{}
	fatal   chan error
	files   chan struct{}

	initializing     bool
	pendingSatisfied bool
	rootSatisfied    bool
	closed           bool
	fatalErr         error
	filesStopped     bool
	groups           []groupState
	conditionIndex   map[conditionKey][]memberRef
	filePaths        map[string]*filePathState

	started          bool
	durationsStarted bool
	timed            *timedReleaseMonitor
	signals          *releaseMonitor
	now              func() time.Time
	newTimer         func(time.Duration) (<-chan time.Time, func())
	fileProbe        fileProbe
	filePollInterval time.Duration
}

// New builds an engine from typed conditions. Group and member slices are
// copied so the caller may reuse or modify its plan after construction.
func New(groups []Group, options Options) (*Engine, error) {
	clone := make([]Group, len(groups))
	for index, group := range groups {
		clone[index].Members = slices.Clone(group.Members)
	}
	engine := newEngineWithGroups(true, clone)
	engine.now = options.Now
	if engine.now == nil {
		engine.now = time.Now
	}
	engine.newTimer = options.NewTimer
	if engine.newTimer == nil {
		engine.newTimer = func(delay time.Duration) (<-chan time.Time, func()) {
			timer := time.NewTimer(delay)
			return timer.C, func() { timer.Stop() }
		}
	}
	engine.fileProbe = options.FileProbe
	if engine.fileProbe == nil {
		engine.fileProbe = probeFileRelease
	}
	engine.filePollInterval = options.FilePollInterval
	if engine.filePollInterval <= 0 {
		engine.filePollInterval = filePollInterval
	}
	return engine, nil
}

// Start begins signal and datetime monitoring, completes the initial file
// probe barrier, and then evaluates immediate duration conditions.
func (engine *Engine) Start() error {
	if engine == nil {
		return fmt.Errorf("condition engine is nil")
	}
	engine.mu.Lock()
	if engine.started {
		engine.mu.Unlock()
		return nil
	}
	engine.started = true
	engine.mu.Unlock()

	configuredSignals := engine.configuredSignals()
	signals, err := newReleaseMonitor(configuredSignals, engine)
	if err != nil {
		engine.Close()
		return err
	}
	engine.signals = signals
	engine.timed = newTimedReleaseMonitor(engine, engine.now, engine.newTimer)
	// Datetimes are armed before probing files so a due datetime becomes a
	// pending selection while all initial probes are still accounted for.
	if err := engine.timed.startDatetimes(); err != nil {
		engine.Close()
		return err
	}
	paths := engine.configuredFiles()
	if len(paths) > 0 {
		monitor, err := newFileMonitorWithProbe(paths, engine, engine.fileProbe, engine.filePollInterval)
		if err != nil {
			engine.Close()
			return err
		}
		_ = monitor
	} else if err := engine.finishInitial(); err != nil {
		engine.Close()
		return err
	}
	if err := engine.fatalError(); err != nil {
		engine.Close()
		return err
	}
	if engine.hasImmediateDuration() {
		if err := engine.satisfyDuration(0); err != nil {
			engine.Close()
			return err
		}
	}
	return nil
}

// StartDurations starts all distinct duration values from one shared instant.
func (engine *Engine) StartDurations() error {
	if engine == nil || engine.timed == nil {
		return nil
	}
	return engine.timed.startDurations()
}

// Release is retained as a compatibility synonym for Satisfied.
func (engine *Engine) Release() <-chan struct{} {
	if engine == nil {
		return nil
	}
	return engine.release
}

// Failures reports the first monitor failure before selection or completion.
func (engine *Engine) Failures() <-chan error {
	if engine == nil {
		return nil
	}
	return engine.fatal
}

// Satisfied closes when an alternative group has become satisfied. Release is
// retained as a synonym for the runtime adapter's transition vocabulary.
func (engine *Engine) Satisfied() <-chan struct{} {
	if engine == nil {
		return nil
	}
	return engine.release
}

// Close stops every monitor. It is safe to call repeatedly. Runtime code must
// keep the signal monitor alive after root satisfaction and call Close only
// when the process is actually finishing.
func (engine *Engine) Close() {
	if engine == nil {
		return
	}
	engine.mu.Lock()
	if engine.closed {
		engine.mu.Unlock()
		return
	}
	engine.closed = true
	engine.stopFilesLocked()
	timed := engine.timed
	signals := engine.signals
	engine.mu.Unlock()
	if timed != nil {
		timed.Close()
	}
	if signals != nil {
		signals.Close()
	}
}

// CompleteEmpty atomically arbitrates empty-input completion against root
// satisfaction. It returns true when selection already won; otherwise it
// closes the engine before any later monitor result can select the root.
func (engine *Engine) CompleteEmpty() (bool, error) {
	if engine == nil {
		return false, nil
	}
	engine.mu.Lock()
	if engine.fatalErr != nil {
		err := engine.fatalErr
		engine.mu.Unlock()
		return false, err
	}
	if engine.rootSatisfied {
		engine.mu.Unlock()
		return true, nil
	}
	if engine.closed {
		engine.mu.Unlock()
		return false, nil
	}
	engine.closed = true
	engine.stopFilesLocked()
	timed := engine.timed
	signals := engine.signals
	engine.mu.Unlock()
	if timed != nil {
		timed.Close()
	}
	if signals != nil {
		signals.Close()
	}
	return false, nil
}

func (engine *Engine) configuredSignals() []string {
	engine.mu.Lock()
	defer engine.mu.Unlock()
	var signals []string
	for _, group := range engine.groups {
		for _, member := range group.members {
			if member.condition.Kind == "signal" {
				signals = append(signals, member.condition.Source)
			}
		}
	}
	return signals
}

func (engine *Engine) configuredFiles() []string {
	engine.mu.Lock()
	defer engine.mu.Unlock()
	var paths []string
	for _, group := range engine.groups {
		for _, member := range group.members {
			if member.condition.Kind == "file" {
				paths = append(paths, member.condition.Source)
			}
		}
	}
	return paths
}

func (engine *Engine) hasImmediateDuration() bool {
	engine.mu.Lock()
	defer engine.mu.Unlock()
	for _, group := range engine.groups {
		for _, member := range group.members {
			if member.condition.Kind == "duration" && member.condition.Duration == 0 {
				return true
			}
		}
	}
	return false
}

func releaseChannelReady(release <-chan struct{}) bool {
	if release == nil {
		return false
	}
	select {
	case <-release:
		return true
	default:
		return false
	}
}

func (engine *Engine) selectRoot() {
	engine.mu.Lock()
	timed := engine.selectRootLocked()
	engine.mu.Unlock()
	if timed != nil {
		timed.Close()
	}
}
