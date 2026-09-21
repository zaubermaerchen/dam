package condition

// This file starts duration and datetime release events and stops their
// timers with the engine's existing OPEN/empty-input lifecycle.

import (
	"slices"
	"strconv"
	"sync"
	"time"
)

type timedReleaseMonitor struct {
	engine   *Engine
	now      func() time.Time
	newTimer func(time.Duration) (<-chan time.Time, func())

	mu               sync.Mutex
	closed           bool
	durationsStarted bool
	datetimesStarted bool
	timers           []*timedReleaseTimer
}

type timedReleaseTimer struct {
	mu        sync.Mutex
	stopped   bool
	stopTimer func()
	done      chan struct{}
}

func newTimedReleaseMonitor(engine *Engine, now func() time.Time, newTimer func(time.Duration) (<-chan time.Time, func())) *timedReleaseMonitor {
	if engine == nil {
		return nil
	}
	if now == nil {
		now = time.Now
	}
	if newTimer == nil {
		newTimer = func(delay time.Duration) (<-chan time.Time, func()) {
			timer := time.NewTimer(delay)
			return timer.C, func() { timer.Stop() }
		}
	}
	return &timedReleaseMonitor{
		engine:   engine,
		now:      now,
		newTimer: newTimer,
	}
}

func (monitor *timedReleaseMonitor) startDatetimes() error {
	if monitor == nil || monitor.engine == nil {
		return nil
	}
	monitor.mu.Lock()
	if monitor.closed || monitor.datetimesStarted {
		monitor.mu.Unlock()
		return nil
	}
	monitor.datetimesStarted = true
	monitor.mu.Unlock()

	_, datetimes := monitor.engine.timedConditions()
	startedAt := monitor.now()
	for _, deadline := range datetimes {
		if err := monitor.startEventAt("datetime", datetimeReleaseKey(deadline), deadline, startedAt); err != nil {
			monitor.Close()
			return err
		}
	}
	return nil
}

func (monitor *timedReleaseMonitor) startDurations() error {
	if monitor == nil || monitor.engine == nil {
		return nil
	}
	monitor.mu.Lock()
	if monitor.closed || monitor.durationsStarted {
		monitor.mu.Unlock()
		return nil
	}
	monitor.durationsStarted = true
	monitor.mu.Unlock()

	durations, _ := monitor.engine.timedConditions()
	startedAt := monitor.now()
	for _, duration := range durations {
		if err := monitor.startEventAt("duration", durationReleaseKey(duration), startedAt.Add(duration), startedAt); err != nil {
			monitor.Close()
			return err
		}
	}
	return nil
}

func (monitor *timedReleaseMonitor) startEvent(kind, source string, target time.Time) error {
	if monitor == nil {
		return nil
	}
	return monitor.startEventAt(kind, source, target, monitor.now())
}

func (monitor *timedReleaseMonitor) startEventAt(kind, source string, target, current time.Time) error {
	if monitor == nil || monitor.engine == nil {
		return nil
	}
	if releaseChannelReady(monitor.engine.release) || monitoringStopped(monitor.engine.files) {
		return nil
	}
	if !target.After(current) {
		return monitor.engine.satisfyCondition(kind, source)
	}

	wait := target.Sub(current)
	capped := false
	if !current.Add(wait).Equal(target) {
		wait = time.Duration(1<<63 - 1)
		capped = true
	}
	timerC, stopTimer := monitor.newTimer(wait)
	if timerC == nil {
		if stopTimer != nil {
			stopTimer()
		}
		return nil
	}
	timer := &timedReleaseTimer{stopTimer: stopTimer, done: make(chan struct{})}
	monitor.mu.Lock()
	if monitor.closed || releaseChannelReady(monitor.engine.release) || monitoringStopped(monitor.engine.files) {
		monitor.mu.Unlock()
		timer.stop()
		return nil
	}
	monitor.timers = append(monitor.timers, timer)
	monitor.mu.Unlock()

	go monitor.waitForEvent(timer, kind, source, target, timerC, capped)
	return nil
}

func (monitor *timedReleaseMonitor) waitForEvent(timer *timedReleaseTimer, kind, source string, target time.Time, timerC <-chan time.Time, capped bool) {
	for {
		select {
		case <-timerC:
			if !timer.fire() {
				return
			}
			current := monitor.now()
			if !capped || !target.After(current) {
				_ = monitor.engine.satisfyCondition(kind, source)
				return
			}
			wait := target.Sub(current)
			capped = false
			if !current.Add(wait).Equal(target) {
				wait = time.Duration(1<<63 - 1)
				capped = true
			}
			nextTimer, stopTimer := monitor.newTimer(wait)
			if nextTimer == nil {
				if stopTimer != nil {
					stopTimer()
				}
				return
			}
			if !timer.arm(stopTimer) {
				return
			}
			timerC = nextTimer
		case <-monitor.engine.release:
			timer.stop()
			return
		case <-monitor.engine.files:
			timer.stop()
			return
		case <-timer.doneChannel():
			return
		}
	}
}

func (timer *timedReleaseTimer) arm(stopTimer func()) bool {
	timer.mu.Lock()
	if timer.stopped {
		timer.mu.Unlock()
		if stopTimer != nil {
			stopTimer()
		}
		return false
	}
	timer.stopTimer = stopTimer
	timer.mu.Unlock()
	return true
}

func (timer *timedReleaseTimer) fire() bool {
	timer.mu.Lock()
	if timer.stopped {
		timer.mu.Unlock()
		return false
	}
	timer.stopTimer = nil
	timer.mu.Unlock()
	return true
}

func (timer *timedReleaseTimer) stop() {
	timer.mu.Lock()
	if timer.stopped {
		timer.mu.Unlock()
		return
	}
	timer.stopped = true
	stopTimer := timer.stopTimer
	timer.stopTimer = nil
	timer.mu.Unlock()
	if stopTimer != nil {
		stopTimer()
	}
	close(timer.done)
}

func (timer *timedReleaseTimer) doneChannel() <-chan struct{} {
	if timer == nil {
		return nil
	}
	return timer.done
}

func (monitor *timedReleaseMonitor) Close() {
	if monitor == nil {
		return
	}
	monitor.mu.Lock()
	if monitor.closed {
		monitor.mu.Unlock()
		return
	}
	monitor.closed = true
	timers := append([]*timedReleaseTimer(nil), monitor.timers...)
	monitor.mu.Unlock()
	for _, timer := range timers {
		timer.stop()
	}
}

func (engine *Engine) timedConditions() ([]time.Duration, []time.Time) {
	if engine == nil {
		return nil, nil
	}
	engine.mu.Lock()
	defer engine.mu.Unlock()

	durationValues := make(map[conditionKey]time.Duration)
	datetimeValues := make(map[conditionKey]time.Time)
	for _, group := range engine.groups {
		for _, member := range group.members {
			switch member.condition.Kind {
			case "duration":
				if value, ok := durationReleaseValue(member.condition); ok {
					durationValues[conditionKeyFor(member.condition)] = value
				}
			case "datetime":
				if value, ok := datetimeReleaseValue(member.condition); ok {
					datetimeValues[conditionKeyFor(member.condition)] = value
				}
			}
		}
	}

	durations := make([]time.Duration, 0, len(durationValues))
	for _, value := range durationValues {
		durations = append(durations, value)
	}
	// Keep timer setup deterministic for tests and diagnostics without making
	// ordering part of the release contract.
	slices.Sort(durations)
	datetimes := make([]time.Time, 0, len(datetimeValues))
	for _, value := range datetimeValues {
		datetimes = append(datetimes, value)
	}
	slices.SortFunc(datetimes, func(left, right time.Time) int {
		if left.Before(right) {
			return -1
		}
		if right.Before(left) {
			return 1
		}
		return 0
	})
	return durations, datetimes
}

func durationReleaseValue(condition Condition) (time.Duration, bool) {
	if condition.Kind == "duration" && condition.Duration >= 0 {
		return condition.Duration, true
	}
	return 0, false
}

func datetimeReleaseValue(condition Condition) (time.Time, bool) {
	if condition.Kind == "datetime" && !condition.Deadline.IsZero() {
		return condition.Deadline, true
	}
	return time.Time{}, false
}

func conditionKeyFor(condition Condition) conditionKey {
	switch condition.Kind {
	case "duration":
		if value, ok := durationReleaseValue(condition); ok {
			return conditionKey{kind: "duration", source: durationReleaseKey(value)}
		}
	case "datetime":
		if value, ok := datetimeReleaseValue(condition); ok {
			return conditionKey{kind: "datetime", source: datetimeReleaseKey(value)}
		}
	}
	return conditionKey{kind: condition.Kind, source: condition.Source}
}

func durationReleaseKey(value time.Duration) string {
	return strconv.FormatInt(int64(value), 10)
}

func datetimeReleaseKey(value time.Time) string {
	return value.UTC().Format(time.RFC3339Nano)
}

func (engine *Engine) satisfyDuration(value time.Duration) error {
	return engine.satisfyCondition("duration", durationReleaseKey(value))
}

func (engine *Engine) satisfyDatetime(value time.Time) error {
	return engine.satisfyCondition("datetime", datetimeReleaseKey(value))
}
