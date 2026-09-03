package main

// This file starts duration and datetime release events and stops their
// timers with the coordinator's existing OPEN/empty-input lifecycle.

import (
	"slices"
	"strconv"
	"sync"
	"time"
)

type timedReleaseMonitor struct {
	coordinator *releaseCoordinator
	now         func() time.Time
	newTimer    func(time.Duration) (<-chan time.Time, func())

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

func newTimedReleaseMonitor(coordinator *releaseCoordinator, now func() time.Time, newTimer func(time.Duration) (<-chan time.Time, func())) *timedReleaseMonitor {
	if coordinator == nil {
		return nil
	}
	if now == nil {
		now = time.Now
	}
	if newTimer == nil {
		newTimer = defaultRuntimeClock().newTimer
	}
	return &timedReleaseMonitor{
		coordinator: coordinator,
		now:         now,
		newTimer:    newTimer,
	}
}

func (monitor *timedReleaseMonitor) startDatetimes() error {
	if monitor == nil || monitor.coordinator == nil {
		return nil
	}
	monitor.mu.Lock()
	if monitor.closed || monitor.datetimesStarted {
		monitor.mu.Unlock()
		return nil
	}
	monitor.datetimesStarted = true
	monitor.mu.Unlock()

	_, datetimes := monitor.coordinator.timedConditions()
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
	if monitor == nil || monitor.coordinator == nil {
		return nil
	}
	monitor.mu.Lock()
	if monitor.closed || monitor.durationsStarted {
		monitor.mu.Unlock()
		return nil
	}
	monitor.durationsStarted = true
	monitor.mu.Unlock()

	durations, _ := monitor.coordinator.timedConditions()
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
	if monitor == nil || monitor.coordinator == nil {
		return nil
	}
	if releaseChannelReady(monitor.coordinator.release) || monitoringStopped(monitor.coordinator.files) {
		return nil
	}
	if !target.After(current) {
		return monitor.coordinator.satisfyCondition(kind, source)
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
	if monitor.closed || releaseChannelReady(monitor.coordinator.release) || monitoringStopped(monitor.coordinator.files) {
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
				_ = monitor.coordinator.satisfyCondition(kind, source)
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
		case <-monitor.coordinator.release:
			timer.stop()
			return
		case <-monitor.coordinator.files:
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

func (coordinator *releaseCoordinator) timedConditions() ([]time.Duration, []time.Time) {
	if coordinator == nil {
		return nil, nil
	}
	coordinator.mu.Lock()
	defer coordinator.mu.Unlock()

	durationValues := make(map[releaseConditionKey]time.Duration)
	datetimeValues := make(map[releaseConditionKey]time.Time)
	for _, group := range coordinator.groups {
		for _, member := range group.members {
			switch member.condition.kind {
			case "duration":
				if value, ok := durationReleaseValue(member.condition); ok {
					durationValues[releaseConditionKeyFor(member.condition)] = value
				}
			case "datetime":
				if value, ok := datetimeReleaseValue(member.condition); ok {
					datetimeValues[releaseConditionKeyFor(member.condition)] = value
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

func durationReleaseValue(condition releaseCondition) (time.Duration, bool) {
	if condition.source != "" {
		value, err := time.ParseDuration(condition.source)
		if err == nil {
			return value, value >= 0
		}
	}
	if condition.duration >= 0 {
		return condition.duration, true
	}
	return 0, false
}

func datetimeReleaseValue(condition releaseCondition) (time.Time, bool) {
	if condition.source != "" {
		value, err := time.Parse(time.RFC3339Nano, condition.source)
		if err == nil {
			return value, true
		}
		if !condition.deadline.IsZero() {
			return condition.deadline, true
		}
		return time.Time{}, false
	}
	return condition.deadline, true
}

func releaseConditionKeyFor(condition releaseCondition) releaseConditionKey {
	switch condition.kind {
	case "duration":
		if value, ok := durationReleaseValue(condition); ok {
			return releaseConditionKey{kind: "duration", source: durationReleaseKey(value)}
		}
	case "datetime":
		if value, ok := datetimeReleaseValue(condition); ok {
			return releaseConditionKey{kind: "datetime", source: datetimeReleaseKey(value)}
		}
	}
	return releaseConditionKey{kind: condition.kind, source: condition.source}
}

func durationReleaseKey(value time.Duration) string {
	return strconv.FormatInt(int64(value), 10)
}

func datetimeReleaseKey(value time.Time) string {
	return value.UTC().Format(time.RFC3339Nano)
}

func (coordinator *releaseCoordinator) satisfyDuration(value time.Duration) error {
	return coordinator.satisfyCondition("duration", durationReleaseKey(value))
}

func (coordinator *releaseCoordinator) satisfyDatetime(value time.Time) error {
	return coordinator.satisfyCondition("datetime", datetimeReleaseKey(value))
}
