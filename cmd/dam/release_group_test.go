package main

// This file verifies compound release groups and their latched event
// semantics without depending on a particular signal-capable target.

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestParseConfigBuildsCompoundReleaseGroupsInConfigurationOrder(t *testing.T) {
	config, err := parseConfig([]string{
		"signal:USR1 && file:first",
		"--or",
		"duration:250ms",
		"--or=file:second && signal:SIGUSR2",
	})
	if err != nil {
		t.Fatalf("parseConfig returned error: %v", err)
	}
	want := []releaseGroup{
		{members: []releaseCondition{
			{kind: "signal", source: "SIGUSR1"},
			{kind: "file", source: "first"},
		}},
		{members: []releaseCondition{newDurationReleaseCondition(250 * time.Millisecond)}},
		{members: []releaseCondition{
			{kind: "file", source: "second"},
			{kind: "signal", source: "SIGUSR2"},
		}},
	}
	if !reflect.DeepEqual(config.groups, want) {
		t.Fatalf("groups = %#v, want %#v", config.groups, want)
	}
	if got, want := config.signals, []string{"SIGUSR1", "SIGUSR2"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("signals = %v, want %v", got, want)
	}
	if got, want := config.files, []string{"first", "second"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("files = %v, want %v", got, want)
	}
	if config.delay == nil || *config.delay != 250*time.Millisecond {
		t.Fatalf("delay = %v, want 250ms", config.delay)
	}
}

func TestParseConfigAcceptsPositionalConditionsAndExplicitOR(t *testing.T) {
	config, err := parseConfigAt([]string{
		"--buffer-size=1K",
		"duration:250ms",
		"--or",
		"datetime:2026-12-31T23:59:07",
		"--buffer-size",
		"2K",
		"--or=signal:SIGUSR1 && file:ready",
	}, time.UTC)
	if err != nil {
		t.Fatalf("parseConfig returned error: %v", err)
	}
	want := []releaseGroup{
		{members: []releaseCondition{newDurationReleaseCondition(250 * time.Millisecond)}},
		{members: []releaseCondition{newDatetimeReleaseCondition(time.Date(2026, time.December, 31, 23, 59, 7, 0, time.UTC))}},
		{members: []releaseCondition{
			{kind: "signal", source: "SIGUSR1"},
			{kind: "file", source: "ready"},
		}},
	}
	if !reflect.DeepEqual(config.groups, want) {
		t.Fatalf("groups = %#v, want %#v", config.groups, want)
	}
	if got, want := config.signals, []string{"SIGUSR1"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("signals = %v, want %v", got, want)
	}
	if got, want := config.files, []string{"ready"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("files = %v, want %v", got, want)
	}
	if got, want := config.bufferSize, 2*1024; got != want {
		t.Fatalf("buffer size = %d, want %d", got, want)
	}
}

func TestParseConfigAcceptsTimedMembersInsideANDGroups(t *testing.T) {
	config, err := parseConfig([]string{
		"duration:1s && datetime:2026-12-31T23:59",
		"--or=signal:USR2 && duration:2s",
	})
	if err != nil {
		t.Fatalf("parseConfig returned error: %v", err)
	}
	if len(config.groups) != 2 || len(config.groups[0].members) != 2 || len(config.groups[1].members) != 2 {
		t.Fatalf("groups = %#v, want two two-member groups", config.groups)
	}
	if got, want := config.groups[0].members[0].kind, "duration"; got != want {
		t.Fatalf("first group first member kind = %q, want %q", got, want)
	}
	if got, want := config.groups[0].members[1].kind, "datetime"; got != want {
		t.Fatalf("first group second member kind = %q, want %q", got, want)
	}
	if got, want := config.groups[1].members[0].source, "SIGUSR2"; got != want {
		t.Fatalf("second group first member source = %q, want %q", got, want)
	}
	if got, want := config.groups[1].members[1].duration, 2*time.Second; got != want {
		t.Fatalf("second group second member duration = %s, want %s", got, want)
	}
}

func TestParseConfigTracksImmediateDurationRegardlessOfConditionOrder(t *testing.T) {
	config, err := parseConfig([]string{"signal:USR1", "--or", "duration:1s", "--or", "duration:0s"})
	if err != nil {
		t.Fatalf("parseConfig returned error: %v", err)
	}
	if !config.immediateDuration {
		t.Fatal("config did not track an immediate duration")
	}
}

func TestParseConfigRejectsMissingOrAdjacentORConditionsAndRemovedSyntax(t *testing.T) {
	tests := [][]string{
		{"--or", "signal:USR1"},
		{"signal:USR1", "--or"},
		{"signal:USR1", "--or="},
		{"signal:USR1", "--or", "--buffer-size", "1K"},
		{"signal:USR1", "file:ready"},
		{"--release-on", "signal:USR1"},
		{"--release-on=signal:USR1"},
		{"1s"},
		{"2026-12-31T23:59"},
	}
	for _, args := range tests {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			if _, err := parseConfig(args); err == nil {
				t.Fatal("parseConfig unexpectedly accepted removed or incomplete syntax")
			}
		})
	}
}

func TestParseConfigCompoundReleaseUsesExactSeparatorWithoutTrimming(t *testing.T) {
	config, err := parseConfig([]string{"file:ready  && signal:USR1"})
	if err != nil {
		t.Fatalf("parseConfig returned error: %v", err)
	}
	if got, want := config.groups[0].members[0].source, "ready "; got != want {
		t.Fatalf("first file source = %q, want %q", got, want)
	}
	pathConfig, err := parseConfig([]string{"file:a&&b"})
	if err != nil {
		t.Fatalf("parseConfig rejected non-separator ampersands: %v", err)
	}
	if got, want := pathConfig.groups[0].members[0].source, "a&&b"; got != want {
		t.Fatalf("non-separator path = %q, want %q", got, want)
	}

	for _, value := range []string{
		" && file:ready",
		"file:ready && ",
		"file:first &&  && file:second",
	} {
		t.Run(value, func(t *testing.T) {
			if _, err := parseConfig([]string{value}); err == nil {
				t.Fatal("parseConfig unexpectedly accepted an empty compound member")
			}
		})
	}
}

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
