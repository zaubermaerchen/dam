package main

// This file covers issue #30 integration boundaries that are not exercised by
// the focused parser, coordinator, forwarding, and signal tests.

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func TestIssue30MixedConditionKindsLatchAcrossANDGroup(t *testing.T) {
	for _, test := range []struct {
		name    string
		missing string
	}{
		{name: "duration withheld", missing: "duration"},
		{name: "datetime withheld", missing: "datetime"},
		{name: "signal withheld", missing: "signal"},
		{name: "file withheld", missing: "file"},
		{name: "all members", missing: ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			dir := t.TempDir()
			first := filepath.Join(dir, "first")
			deadline := time.Date(2026, time.January, 2, 3, 4, 5, 0, time.UTC)
			config, err := parseConfigAt([]string{
				"duration:10s && datetime:2026-01-02T03:04:05 && signal:USR1 && file:" + first,
			}, time.UTC)
			if err != nil {
				t.Fatalf("parseConfigAt returned error: %v", err)
			}
			coordinator := newReleaseCoordinatorWithGroups(false, config.releaseGroups())
			t.Cleanup(coordinator.stopFiles)

			// Every row withholds exactly one member. The all-members row
			// confirms that the same event sequence does open the gate.
			events := []struct {
				kind    string
				satisfy func() error
			}{
				{kind: "file", satisfy: func() error { return coordinator.reportFileReady(first) }},
				{kind: "signal", satisfy: func() error { return coordinator.satisfySignal("SIGUSR1") }},
				{kind: "datetime", satisfy: func() error { return coordinator.satisfyDatetime(deadline) }},
				{kind: "duration", satisfy: func() error { return coordinator.satisfyDuration(10 * time.Second) }},
			}
			for _, event := range events {
				if event.kind == test.missing {
					continue
				}
				if err := event.satisfy(); err != nil {
					t.Fatalf("satisfy %s returned error: %v", event.kind, err)
				}
			}

			if test.missing == "" {
				select {
				case <-coordinator.release:
				default:
					t.Fatal("mixed AND group did not open after every member was satisfied")
				}
				return
			}
			select {
			case <-coordinator.release:
				t.Fatalf("mixed AND group opened with %s withheld", test.missing)
			default:
			}
		})
	}
}

func TestIssue30RunMixedTimedAndFileGroupsStopsAlternativeMonitorsAfterOpen(t *testing.T) {
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

	input := &issue30BlockingAfterFirstReader{data: []byte("mixed timed/file payload"), release: make(chan struct{})}
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

type issue30BlockingAfterFirstReader struct {
	data    []byte
	release chan struct{}
	mu      sync.Mutex
	reads   int
}

func (reader *issue30BlockingAfterFirstReader) Read(buffer []byte) (int, error) {
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

func TestIssue30InitialFatalWinsPendingTimedSignalAndReadyFile(t *testing.T) {
	fatal := errors.New("initial release probe failed")
	deadline := time.Date(2026, time.January, 2, 3, 4, 5, 0, time.UTC)
	coordinator := newReleaseCoordinatorWithGroups(true, []releaseGroup{
		{members: []releaseCondition{
			newDurationReleaseCondition(time.Second),
			newDatetimeReleaseCondition(deadline),
			{kind: "signal", source: "SIGUSR1"},
			{kind: "file", source: "ready"},
		}},
		{members: []releaseCondition{{kind: "file", source: "bad"}}},
	})
	monitor, err := newFileMonitorWithProbe([]string{"ready", "bad"}, coordinator, func(path string) (bool, error) {
		if path == "ready" {
			if err := coordinator.satisfyDuration(time.Second); err != nil {
				return false, err
			}
			if err := coordinator.satisfyDatetime(deadline); err != nil {
				return false, err
			}
			if err := coordinator.satisfySignal("SIGUSR1"); err != nil {
				return false, err
			}
			return true, nil
		}
		return false, fatal
	}, time.Millisecond)
	if monitor == nil {
		t.Fatal("newFileMonitorWithProbe returned nil monitor")
	}
	t.Cleanup(monitor.Close)
	if !errors.Is(err, fatal) {
		t.Fatalf("initial error = %v, want %v", err, fatal)
	}
	if got := coordinator.fatalError(); !errors.Is(got, fatal) {
		t.Fatalf("coordinator fatal = %v, want %v", got, fatal)
	}
	select {
	case <-coordinator.release:
		t.Fatal("pending timed/signal/file events opened gate despite initial fatal")
	default:
	}
}

func TestIssue30InFlightFileFatalIsIgnoredAfterTimedOpen(t *testing.T) {
	coordinator := newReleaseCoordinatorWithGroups(false, []releaseGroup{
		{members: []releaseCondition{newDurationReleaseCondition(time.Second)}},
	})
	t.Cleanup(coordinator.stopFiles)
	probeStarted := make(chan struct{})
	allowProbe := make(chan struct{})
	monitor := &fileMonitor{
		coordinator: coordinator,
		interval:    time.Millisecond,
		wait: func(string, <-chan struct{}, time.Duration) bool {
			return true
		},
		probe: func(string) (bool, error) {
			close(probeStarted)
			<-allowProbe
			return false, errors.New("late file probe failure")
		},
	}
	done := make(chan struct{})
	go func() {
		monitor.watchPath("late")
		close(done)
	}()
	select {
	case <-probeStarted:
	case <-time.After(testTimeout):
		t.Fatal("in-flight file probe did not start")
	}
	if err := coordinator.satisfyDuration(time.Second); err != nil {
		t.Fatalf("satisfyDuration returned error: %v", err)
	}
	select {
	case <-coordinator.release:
	case <-time.After(testTimeout):
		t.Fatal("timed condition did not open coordinator")
	}
	close(allowProbe)
	select {
	case <-done:
	case <-time.After(testTimeout):
		t.Fatal("file watcher did not finish after in-flight probe")
	}
	if got := coordinator.fatalError(); got != nil {
		t.Fatalf("late file fatal = %v, want nil after OPEN", got)
	}
}

func TestIssue30TimedAndFileFatalOpenBoundaryHasConsistentOutcome(t *testing.T) {
	for attempt := 0; attempt < 100; attempt++ {
		coordinator := newReleaseCoordinatorWithGroups(false, []releaseGroup{
			{members: []releaseCondition{newDurationReleaseCondition(time.Second)}},
			{members: []releaseCondition{{kind: "file", source: "late"}}},
		})
		probeStarted := make(chan struct{})
		allowProbe := make(chan struct{})
		monitor := &fileMonitor{
			coordinator: coordinator,
			interval:    time.Millisecond,
			wait: func(string, <-chan struct{}, time.Duration) bool {
				return true
			},
			probe: func(string) (bool, error) {
				close(probeStarted)
				<-allowProbe
				return false, errors.New("barrier file probe failure")
			},
		}
		watchDone := make(chan struct{})
		go func() {
			monitor.watchPath("late")
			close(watchDone)
		}()
		select {
		case <-probeStarted:
		case <-time.After(testTimeout):
			coordinator.stopFiles()
			t.Fatalf("attempt %d: file probe did not reach the barrier", attempt)
		}

		start := make(chan struct{})
		timedDone := make(chan error, 1)
		go func() {
			<-start
			timedDone <- coordinator.satisfyDuration(time.Second)
		}()
		go func() {
			<-start
			close(allowProbe)
		}()
		close(start)

		if err := <-timedDone; err != nil && !errors.Is(err, coordinator.fatalError()) {
			t.Fatalf("attempt %d: timed event error = %v, fatal = %v", attempt, err, coordinator.fatalError())
		}
		select {
		case <-watchDone:
		case <-time.After(testTimeout):
			coordinator.stopFiles()
			t.Fatalf("attempt %d: file watcher did not finish", attempt)
		}

		opened := releaseChannelReady(coordinator.release)
		fatal := coordinator.fatalError()
		if opened == (fatal != nil) {
			coordinator.stopFiles()
			t.Fatalf("attempt %d: inconsistent OPEN/fatal state: opened=%t fatal=%v", attempt, opened, fatal)
		}
		coordinator.stopFiles()
	}
}
