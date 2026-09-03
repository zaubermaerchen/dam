package main

// This file verifies timed release conditions share the coordinator's latch
// and monitor lifecycle with signal and file conditions.

import (
	"sync"
	"testing"
	"time"
)

func TestTimedReleaseConditionsNormalizeAndFanOutEquivalentDurations(t *testing.T) {
	coordinator := newReleaseCoordinatorWithGroups(false, []releaseGroup{
		{members: []releaseCondition{
			{kind: "duration", source: "60s"},
			{kind: "signal", source: "SIGUSR1"},
		}},
		{members: []releaseCondition{
			{kind: "duration", source: "1m"},
			{kind: "signal", source: "SIGUSR2"},
		}},
	})
	t.Cleanup(coordinator.stopFiles)

	if err := coordinator.satisfyDuration(time.Minute); err != nil {
		t.Fatalf("satisfyDuration returned error: %v", err)
	}
	if coordinator.groups[0].remaining != 1 || coordinator.groups[1].remaining != 1 {
		t.Fatalf("duration event did not fan out: remaining = %d, %d", coordinator.groups[0].remaining, coordinator.groups[1].remaining)
	}
	if err := coordinator.satisfySignal("SIGUSR1"); err != nil {
		t.Fatalf("satisfySignal returned error: %v", err)
	}
	select {
	case <-coordinator.release:
	case <-time.After(testTimeout):
		t.Fatal("equivalent duration condition did not complete its group")
	}
}

func TestTimedReleaseConditionsNormalizeEquivalentDatetimes(t *testing.T) {
	instant := time.Date(2026, time.January, 2, 3, 4, 5, 0, time.FixedZone("offset", 2*60*60))
	coordinator := newReleaseCoordinatorWithGroups(false, []releaseGroup{
		{members: []releaseCondition{
			{kind: "datetime", source: "2026-01-02T03:04:05+02:00"},
			{kind: "signal", source: "SIGUSR1"},
		}},
		{members: []releaseCondition{
			{kind: "datetime", source: "2026-01-02T01:04:05Z"},
			{kind: "signal", source: "SIGUSR2"},
		}},
	})
	t.Cleanup(coordinator.stopFiles)

	if err := coordinator.satisfyDatetime(instant.UTC()); err != nil {
		t.Fatalf("satisfyDatetime returned error: %v", err)
	}
	if coordinator.groups[0].remaining != 1 || coordinator.groups[1].remaining != 1 {
		t.Fatalf("datetime event did not fan out: remaining = %d, %d", coordinator.groups[0].remaining, coordinator.groups[1].remaining)
	}
}

func TestTimedReleaseConditionLatchesEitherMemberOrder(t *testing.T) {
	for _, durationFirst := range []bool{true, false} {
		coordinator := newReleaseCoordinatorWithGroups(false, []releaseGroup{{members: []releaseCondition{
			newDurationReleaseCondition(time.Second),
			{kind: "signal", source: "SIGUSR1"},
		}}})
		if durationFirst {
			if err := coordinator.satisfyDuration(time.Second); err != nil {
				t.Fatal(err)
			}
			if err := coordinator.satisfySignal("SIGUSR1"); err != nil {
				t.Fatal(err)
			}
		} else {
			if err := coordinator.satisfySignal("SIGUSR1"); err != nil {
				t.Fatal(err)
			}
			if err := coordinator.satisfyDuration(time.Second); err != nil {
				t.Fatal(err)
			}
		}
		select {
		case <-coordinator.release:
		case <-time.After(testTimeout):
			t.Fatal("timed AND group did not release in either member order")
		}
	}
}

func TestTimedReleaseMonitorStartsDistinctDurationsFromOneReadEvent(t *testing.T) {
	coordinator := newReleaseCoordinatorWithGroups(false, []releaseGroup{
		{members: []releaseCondition{
			newDurationReleaseCondition(time.Second),
			{kind: "signal", source: "SIGUSR1"},
		}},
		{members: []releaseCondition{
			newDurationReleaseCondition(2 * time.Second),
			{kind: "signal", source: "SIGUSR2"},
		}},
	})
	var (
		mu      sync.Mutex
		delays  []time.Duration
		timers  []chan time.Time
		stopped int
	)
	monitor := newTimedReleaseMonitor(coordinator, func() time.Time {
		return time.Date(2026, time.January, 2, 3, 4, 5, 0, time.UTC)
	}, func(delay time.Duration) (<-chan time.Time, func()) {
		mu.Lock()
		defer mu.Unlock()
		delays = append(delays, delay)
		timer := make(chan time.Time, 1)
		timers = append(timers, timer)
		return timer, func() {
			mu.Lock()
			stopped++
			mu.Unlock()
		}
	})
	if monitor == nil {
		t.Fatal("newTimedReleaseMonitor returned nil")
	}
	t.Cleanup(func() {
		monitor.Close()
		coordinator.stopFiles()
	})

	if err := monitor.startDurations(); err != nil {
		t.Fatalf("startDurations returned error: %v", err)
	}
	mu.Lock()
	gotDelays := append([]time.Duration(nil), delays...)
	gotTimers := append([]chan time.Time(nil), timers...)
	mu.Unlock()
	if len(gotDelays) != 2 || gotDelays[0] != time.Second || gotDelays[1] != 2*time.Second {
		t.Fatalf("timer delays = %v, want [1s 2s]", gotDelays)
	}
	gotTimers[0] <- time.Time{}
	gotTimers[1] <- time.Time{}
	select {
	case <-coordinator.release:
		t.Fatal("duration timers opened incomplete groups")
	default:
	}
	if err := coordinator.satisfySignal("SIGUSR1"); err != nil {
		t.Fatalf("first signal returned error: %v", err)
	}
	if err := coordinator.satisfySignal("SIGUSR2"); err != nil {
		t.Fatalf("second signal returned error: %v", err)
	}
	select {
	case <-coordinator.release:
	case <-time.After(testTimeout):
		t.Fatal("distinct duration events did not release coordinator")
	}
}

func TestTimedReleaseMonitorStartsDistinctDatetimesAtStartup(t *testing.T) {
	startedAt := time.Date(2026, time.January, 2, 3, 4, 5, 0, time.UTC)
	coordinator := newReleaseCoordinatorWithGroups(false, []releaseGroup{
		{members: []releaseCondition{
			newDatetimeReleaseCondition(startedAt.Add(time.Second)),
			{kind: "signal", source: "SIGUSR1"},
		}},
		{members: []releaseCondition{
			newDatetimeReleaseCondition(startedAt.Add(2 * time.Second)),
			{kind: "signal", source: "SIGUSR2"},
		}},
	})
	t.Cleanup(coordinator.stopFiles)
	var (
		mu     sync.Mutex
		delays []time.Duration
		timers []chan time.Time
	)
	monitor := newTimedReleaseMonitor(coordinator, func() time.Time { return startedAt }, func(delay time.Duration) (<-chan time.Time, func()) {
		mu.Lock()
		defer mu.Unlock()
		delays = append(delays, delay)
		timer := make(chan time.Time, 1)
		timers = append(timers, timer)
		return timer, func() {}
	})
	if err := monitor.startDatetimes(); err != nil {
		t.Fatalf("startDatetimes returned error: %v", err)
	}
	mu.Lock()
	gotDelays := append([]time.Duration(nil), delays...)
	gotTimers := append([]chan time.Time(nil), timers...)
	mu.Unlock()
	if len(gotDelays) != 2 || gotDelays[0] != time.Second || gotDelays[1] != 2*time.Second {
		t.Fatalf("timer delays = %v, want [1s 2s]", gotDelays)
	}
	gotTimers[0] <- time.Time{}
	gotTimers[1] <- time.Time{}
	deadline := time.Now().Add(testTimeout)
	for {
		coordinator.mu.Lock()
		ready := coordinator.groups[0].remaining == 1 && coordinator.groups[1].remaining == 1
		coordinator.mu.Unlock()
		if ready {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("datetime timers did not satisfy their members")
		}
		time.Sleep(time.Millisecond)
	}
	if err := coordinator.satisfySignal("SIGUSR1"); err != nil {
		t.Fatalf("first signal returned error: %v", err)
	}
	if err := coordinator.satisfySignal("SIGUSR2"); err != nil {
		t.Fatalf("second signal returned error: %v", err)
	}
	select {
	case <-coordinator.release:
	case <-time.After(testTimeout):
		t.Fatal("distinct datetime events did not release coordinator")
	}
	monitor.Close()
}

func TestTimedReleaseMonitorStopsTimersAfterOpenAndEmptyInput(t *testing.T) {
	for _, complete := range []struct {
		name     string
		duration bool
		start    func(*timedReleaseMonitor) error
		stop     func(*releaseCoordinator) error
	}{
		{name: "datetime-open", start: func(monitor *timedReleaseMonitor) error {
			return monitor.startDatetimes()
		}, stop: func(coordinator *releaseCoordinator) error {
			return coordinator.requestOpen()
		}},
		{name: "datetime-empty", start: func(monitor *timedReleaseMonitor) error {
			return monitor.startDatetimes()
		}, stop: func(coordinator *releaseCoordinator) error {
			return coordinator.completeEmpty()
		}},
		{name: "duration-open", duration: true, start: func(monitor *timedReleaseMonitor) error {
			return monitor.startDurations()
		}, stop: func(coordinator *releaseCoordinator) error {
			return coordinator.requestOpen()
		}},
		{name: "duration-empty", duration: true, start: func(monitor *timedReleaseMonitor) error {
			return monitor.startDurations()
		}, stop: func(coordinator *releaseCoordinator) error {
			return coordinator.completeEmpty()
		}},
	} {
		t.Run(complete.name, func(t *testing.T) {
			condition := newDatetimeReleaseCondition(time.Date(2026, time.January, 2, 3, 4, 6, 0, time.UTC))
			if complete.duration {
				condition = newDurationReleaseCondition(time.Second)
			}
			coordinator := newReleaseCoordinatorWithGroups(false, []releaseGroup{{members: []releaseCondition{condition}}})
			t.Cleanup(coordinator.stopFiles)
			stopped := make(chan struct{})
			monitor := newTimedReleaseMonitor(coordinator, func() time.Time {
				return time.Date(2026, time.January, 2, 3, 4, 5, 0, time.UTC)
			}, func(time.Duration) (<-chan time.Time, func()) {
				return make(chan time.Time), func() { close(stopped) }
			})
			if monitor == nil {
				t.Fatal("newTimedReleaseMonitor returned nil")
			}
			t.Cleanup(monitor.Close)
			if err := complete.start(monitor); err != nil {
				t.Fatalf("startDatetimes returned error: %v", err)
			}
			if err := complete.stop(coordinator); err != nil {
				t.Fatalf("stop transition returned error: %v", err)
			}
			select {
			case <-stopped:
			case <-time.After(testTimeout):
				t.Fatal("timed monitor did not stop its outstanding timer")
			}
		})
	}
}
