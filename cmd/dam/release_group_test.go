package main

// This file verifies compound release groups and their latched event
// semantics without depending on a particular signal-capable target.

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestReleaseCoordinatorCompoundGroupsLatchAndFanOutDuplicateEvents(t *testing.T) {
	coordinator := newReleaseCoordinatorWithGroups(false, []releaseGroup{
		{members: []releaseCondition{
			{kind: "signal", source: "SIGUSR1"},
			{kind: "signal", source: "SIGUSR1"},
			{kind: "file", source: "ready"},
			{kind: "file", source: "ready"},
		}},
	})
	t.Cleanup(coordinator.stopFiles)

	if err := coordinator.satisfySignal("SIGUSR1"); err != nil {
		t.Fatalf("satisfySignal returned error: %v", err)
	}
	select {
	case <-coordinator.release:
		t.Fatal("signal event opened an incomplete group")
	default:
	}
	if err := coordinator.reportFileReady("ready"); err != nil {
		t.Fatalf("reportFileReady returned error: %v", err)
	}
	select {
	case <-coordinator.release:
	case <-time.After(testTimeout):
		t.Fatal("duplicate fan-out did not satisfy the compound group")
	}

	// Latches remain satisfied: deleting/replacing the file does not close an
	// already-open gate, and repeated events are harmless.
	if err := coordinator.satisfySignal("SIGUSR1"); err != nil {
		t.Fatalf("repeated satisfySignal returned error: %v", err)
	}
	if err := coordinator.reportFileReady("ready"); err != nil {
		t.Fatalf("repeated reportFileReady returned error: %v", err)
	}
}

func TestReleaseCoordinatorCompoundGroupsUseORAcrossOptions(t *testing.T) {
	coordinator := newReleaseCoordinatorWithGroups(false, []releaseGroup{
		{members: []releaseCondition{
			{kind: "file", source: "a"},
			{kind: "file", source: "b"},
		}},
		{members: []releaseCondition{
			{kind: "signal", source: "SIGUSR1"},
		}},
	})
	t.Cleanup(coordinator.stopFiles)

	if err := coordinator.satisfySignal("SIGUSR1"); err != nil {
		t.Fatalf("satisfySignal returned error: %v", err)
	}
	select {
	case <-coordinator.release:
	case <-time.After(testTimeout):
		t.Fatal("single-member alternative group did not open")
	}
}

func TestInitialCompoundFileProbeErrorsAreRetried(t *testing.T) {
	coordinator := newReleaseCoordinatorWithGroups(true, []releaseGroup{
		{members: []releaseCondition{{kind: "file", source: "first"}}},
		{members: []releaseCondition{{kind: "file", source: "second"}}},
	})
	t.Cleanup(coordinator.stopFiles)
	var mu sync.Mutex
	calls := make(map[string]int)
	monitor, err := newFileMonitorWithProbe([]string{"first", "second"}, coordinator, func(path string) (bool, error) {
		mu.Lock()
		calls[path]++
		call := calls[path]
		mu.Unlock()
		if call == 1 {
			return false, errors.New("transient initial probe failure")
		}
		return path == "first", nil
	}, time.Millisecond)
	if monitor == nil {
		t.Fatal("newFileMonitorWithProbe returned nil monitor")
	}
	t.Cleanup(monitor.Close)
	if err != nil {
		t.Fatalf("newFileMonitorWithProbe returned error: %v", err)
	}
	if got := coordinator.fatalError(); got != nil {
		t.Fatalf("initial probe error became fatal: %v", got)
	}
	select {
	case <-coordinator.release:
	case <-time.After(testTimeout):
		t.Fatal("retryable initial probe did not release after the file became regular")
	}
	mu.Lock()
	firstCalls := calls["first"]
	mu.Unlock()
	if firstCalls < 2 {
		t.Fatalf("first probe calls = %d, want an initial error followed by a retry", firstCalls)
	}
}

func TestNonRegularCompoundFileMemberRemainsPendingUntilRegular(t *testing.T) {
	dir := t.TempDir()
	bad := filepath.Join(dir, "bad")
	other := filepath.Join(dir, "other")
	if err := os.Mkdir(bad, 0o700); err != nil {
		t.Fatalf("create initial non-regular path: %v", err)
	}
	input := eofReader{data: []byte("held")}
	output := &lockedBuffer{writeTimes: make(chan time.Time, 1)}
	var diagnostics bytes.Buffer
	status := make(chan int, 1)
	finished := make(chan struct{})
	args := []string{"file:" + bad + " && file:" + other}
	t.Cleanup(func() {
		if err := os.RemoveAll(bad); err == nil {
			_ = os.WriteFile(bad, []byte("cleanup"), 0o600)
		}
		if err := os.RemoveAll(other); err == nil {
			_ = os.WriteFile(other, []byte("cleanup"), 0o600)
		}
		select {
		case <-finished:
		case <-time.After(testTimeout):
			t.Error("compound file invocation did not finish during cleanup")
		}
	})
	go func() {
		status <- run(args, input, output, &diagnostics)
		close(finished)
	}()
	select {
	case got := <-status:
		t.Fatalf("compound file invocation completed before its members were regular: %d", got)
	case <-output.writeTimes:
		t.Fatal("compound file condition released with a non-regular member")
	case <-time.After(50 * time.Millisecond):
	}
	if err := os.Remove(bad); err != nil {
		t.Fatalf("remove non-regular member: %v", err)
	}
	if err := os.WriteFile(bad, []byte("ready"), 0o600); err != nil {
		t.Fatalf("create regular member: %v", err)
	}
	select {
	case got := <-status:
		t.Fatalf("compound file invocation completed with one AND member missing: %d", got)
	case <-output.writeTimes:
		t.Fatal("compound file condition released with one AND member missing")
	case <-time.After(50 * time.Millisecond):
	}
	if err := os.WriteFile(other, []byte("ready"), 0o600); err != nil {
		t.Fatalf("create second regular member: %v", err)
	}
	select {
	case got := <-status:
		if got != 0 {
			t.Fatalf("compound file status = %d, diagnostics = %q", got, diagnostics.String())
		}
	case <-time.After(testTimeout):
		t.Fatal("compound file condition did not release after both members became regular")
	}
	if got, want := output.String(), "held"; got != want {
		t.Fatalf("compound file output = %q, want %q", got, want)
	}
	if diagnostics.Len() != 0 {
		t.Fatalf("unexpected diagnostics: %q", diagnostics.String())
	}
}

func TestSatisfiedFileLatchIgnoresLaterFatalProbe(t *testing.T) {
	fatal := errors.New("file replaced after latch")
	coordinator := newReleaseCoordinatorWithGroups(false, []releaseGroup{
		{members: []releaseCondition{
			{kind: "file", source: "first"},
			{kind: "file", source: "second"},
		}},
	})
	t.Cleanup(coordinator.stopFiles)
	if err := coordinator.reportFileReady("first"); err != nil {
		t.Fatalf("reportFileReady returned error: %v", err)
	}
	coordinator.reportFileFatal("first", fatal)
	if got := coordinator.fatalError(); got != nil {
		t.Fatalf("fatal after satisfied file latch = %v, want nil", got)
	}
	if err := coordinator.reportFileReady("second"); err != nil {
		t.Fatalf("reportFileReady returned error: %v", err)
	}
	select {
	case <-coordinator.release:
	case <-time.After(testTimeout):
		t.Fatal("remaining file member did not complete group")
	}
}

func TestRunFileAndFileGroupWaitsForEveryMember(t *testing.T) {
	dir := t.TempDir()
	first := filepath.Join(dir, "first")
	second := filepath.Join(dir, "second")
	input := eofReader{data: []byte("held until both files")}
	output := &lockedBuffer{writeTimes: make(chan time.Time, 1)}
	var diagnostics bytes.Buffer
	status := make(chan int, 1)
	go func() {
		status <- run([]string{"file:" + first + " && file:" + second}, input, output, &diagnostics)
	}()

	select {
	case <-output.writeTimes:
		t.Fatal("compound file condition released before either file appeared")
	case <-time.After(50 * time.Millisecond):
	}
	if err := os.WriteFile(first, []byte("ready"), 0o600); err != nil {
		t.Fatal(err)
	}
	select {
	case <-output.writeTimes:
		t.Fatal("compound file condition released after only one member")
	case <-time.After(50 * time.Millisecond):
	}
	if err := os.WriteFile(second, []byte("ready"), 0o600); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-status:
		if got != 0 {
			t.Fatalf("run status = %d, diagnostics = %q", got, diagnostics.String())
		}
	case <-time.After(testTimeout):
		t.Fatal("compound file condition did not release")
	}
	if got, want := output.String(), "held until both files"; got != want {
		t.Fatalf("output = %q, want %q", got, want)
	}
}

func TestRunCompoundFileGroupKeepsEmptyEOFCompatibility(t *testing.T) {
	dir := t.TempDir()
	args := []string{"file:" + filepath.Join(dir, "first") + " && file:" + filepath.Join(dir, "second")}
	var output, diagnostics bytes.Buffer
	status := make(chan int, 1)
	go func() {
		status <- run(args, strings.NewReader(""), &output, &diagnostics)
	}()
	select {
	case got := <-status:
		if got != 0 {
			t.Fatalf("empty compound input status = %d, diagnostics = %q", got, diagnostics.String())
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("empty compound input waited for an incomplete group")
	}
	if output.Len() != 0 {
		t.Fatalf("empty compound input wrote output: %q", output.String())
	}
}

func TestRunCompoundFileGroupKeepsDeadlineCompatibility(t *testing.T) {
	dir := t.TempDir()
	args := []string{
		"duration:20ms",
		"--or",
		"file:" + filepath.Join(dir, "first") + " && file:" + filepath.Join(dir, "second"),
	}
	var output, diagnostics bytes.Buffer
	status := run(args, eofReader{data: []byte("deadline released")}, &output, &diagnostics)
	if status != 0 {
		t.Fatalf("compound deadline status = %d, diagnostics = %q", status, diagnostics.String())
	}
	if got, want := output.String(), "deadline released"; got != want {
		t.Fatalf("compound deadline output = %q, want %q", got, want)
	}
}

func TestMixedConditionKindsLatchAcrossANDGroup(t *testing.T) {
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

func TestInitialProbeErrorDoesNotOverrideSatisfiedOtherGroup(t *testing.T) {
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
		return false, errors.New("transient initial probe failure")
	}, time.Millisecond)
	if monitor == nil {
		t.Fatal("newFileMonitorWithProbe returned nil monitor")
	}
	t.Cleanup(monitor.Close)
	if err != nil {
		t.Fatalf("newFileMonitorWithProbe returned error: %v", err)
	}
	if got := coordinator.fatalError(); got != nil {
		t.Fatalf("initial probe error became fatal: %v", got)
	}
	select {
	case <-coordinator.release:
	case <-time.After(testTimeout):
		t.Fatal("satisfied alternative group did not open the gate")
	}
}
