package main

// This file verifies timed release conditions share the coordinator's latch
// and monitor lifecycle with signal and file conditions.

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
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

func TestRunMixedTimedAndFileGroupsStopsAlternativeMonitorsAfterOpen(t *testing.T) {
	now := time.Date(2026, time.January, 2, 3, 4, 0, 0, time.UTC)
	dir := t.TempDir()
	first := filepath.Join(dir, "first")
	second := filepath.Join(dir, "second")

	var (
		mu         sync.Mutex
		timers     = make(map[time.Duration]chan time.Time)
		stopped    = make(map[time.Duration]int)
		stopEvents = make(chan time.Duration, 4)
		allTimer   = make(chan struct{})
	)
	clock := runtimeClock{
		now:      func() time.Time { return now },
		location: time.UTC,
		newTimer: func(delay time.Duration) (<-chan time.Time, func()) {
			mu.Lock()
			channel := make(chan time.Time, 1)
			timers[delay] = channel
			if len(timers) == 3 {
				close(allTimer)
			}
			mu.Unlock()
			return channel, func() {
				mu.Lock()
				stopped[delay]++
				mu.Unlock()
				stopEvents <- delay
			}
		},
	}

	input := &blockingAfterFirstReader{data: []byte("mixed timed/file payload"), release: make(chan struct{})}
	output := &lockedBuffer{writeTimes: make(chan time.Time, 1)}
	var diagnostics bytes.Buffer
	status := make(chan int, 1)
	go func() {
		status <- runWithClock([]string{
			"duration:5s && datetime:2026-01-02T03:03:00 && file:" + first,
			"--or",
			"duration:10s && datetime:2026-01-02T03:05:00 && file:" + second,
		}, input, output, &diagnostics, clock)
	}()

	select {
	case <-allTimer:
	case <-time.After(testTimeout):
		t.Fatal("run did not start the distinct datetime and duration timers")
	}
	if err := os.WriteFile(first, []byte("ready"), 0o600); err != nil {
		t.Fatalf("create first release file: %v", err)
	}
	mu.Lock()
	durationTimer := timers[5*time.Second]
	mu.Unlock()
	if durationTimer == nil {
		t.Fatal("run did not create the first duration timer")
	}
	durationTimer <- now

	select {
	case <-output.writeTimes:
	case <-time.After(testTimeout):
		t.Fatal("mixed timed/file group did not write held data after release")
	}
	wantStopped := map[time.Duration]bool{10 * time.Second: false, time.Minute: false}
	for len(wantStopped) > 0 {
		select {
		case got := <-stopEvents:
			if _, ok := wantStopped[got]; !ok {
				t.Fatalf("unexpected stopped timer delay %v", got)
			}
			delete(wantStopped, got)
		case <-time.After(testTimeout):
			t.Fatalf("timers %v were not stopped before the blocked read was released", wantStopped)
		}
	}
	select {
	case got := <-status:
		t.Fatalf("run returned before post-release read was released with status %d", got)
	default:
	}
	close(input.release)
	select {
	case got := <-status:
		if got != 0 {
			t.Fatalf("run status = %d, diagnostics = %q", got, diagnostics.String())
		}
	case <-time.After(testTimeout):
		t.Fatal("mixed timed/file group did not complete after post-release read")
	}
	if got, want := output.String(), string(input.data); got != want {
		t.Fatalf("output = %q, want %q", got, want)
	}
	mu.Lock()
	defer mu.Unlock()
	if stopped[10*time.Second] == 0 {
		t.Fatal("unselected duration timer was not stopped after OPEN")
	}
	if stopped[time.Minute] == 0 {
		t.Fatal("unselected datetime timer was not stopped after OPEN")
	}
}

type blockingAfterFirstReader struct {
	data    []byte
	release chan struct{}
	mu      sync.Mutex
	reads   int
}

func (reader *blockingAfterFirstReader) Read(buffer []byte) (int, error) {
	reader.mu.Lock()
	reader.reads++
	read := reader.reads
	reader.mu.Unlock()
	if read == 1 {
		return copy(buffer, reader.data), nil
	}
	<-reader.release
	return 0, io.EOF
}
