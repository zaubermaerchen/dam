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
	"testing"
	"time"
)

func TestParseConfigBuildsCompoundReleaseGroupsInConfigurationOrder(t *testing.T) {
	config, err := parseConfig([]string{
		"--release-on", "signal:USR1 && file:first",
		"250ms",
		"--release-on=file:second && signal:SIGUSR2",
	})
	if err != nil {
		t.Fatalf("parseConfig returned error: %v", err)
	}
	want := []releaseGroup{
		{members: []releaseCondition{
			{kind: "signal", source: "SIGUSR1"},
			{kind: "file", source: "first"},
		}},
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

func TestParseConfigCompoundReleaseUsesExactSeparatorWithoutTrimming(t *testing.T) {
	config, err := parseConfig([]string{"--release-on", "file:ready  && signal:USR1"})
	if err != nil {
		t.Fatalf("parseConfig returned error: %v", err)
	}
	if got, want := config.groups[0].members[0].source, "ready "; got != want {
		t.Fatalf("first file source = %q, want %q", got, want)
	}
	pathConfig, err := parseConfig([]string{"--release-on", "file:a&&b"})
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
			if _, err := parseConfig([]string{"--release-on", value}); err == nil {
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

func TestInitialCompoundFileProbeFatalWinsAcrossORGroupsInConfigurationOrder(t *testing.T) {
	firstFatal := errors.New("first initial fatal")
	secondFatal := errors.New("second initial fatal")
	coordinator := newReleaseCoordinatorWithGroups(true, []releaseGroup{
		{members: []releaseCondition{{kind: "file", source: "first"}}},
		{members: []releaseCondition{{kind: "file", source: "second"}}},
	})
	monitor, err := newFileMonitorWithProbe([]string{"first", "second"}, coordinator, func(path string) (bool, error) {
		if path == "first" {
			return false, firstFatal
		}
		return false, secondFatal
	}, time.Millisecond)
	if monitor == nil {
		t.Fatal("newFileMonitorWithProbe returned nil monitor")
	}
	if !errors.Is(err, firstFatal) {
		t.Fatalf("initial error = %v, want %v", err, firstFatal)
	}
	if errors.Is(err, secondFatal) {
		t.Fatalf("initial error = %v, selected later configured fatal", err)
	}
	select {
	case <-coordinator.release:
		t.Fatal("initial fatal opened the gate")
	default:
	}
}

func TestRuntimeFatalFromUnsatisfiedCompoundFileFailsInvocation(t *testing.T) {
	dir := t.TempDir()
	bad := filepath.Join(dir, "bad")
	other := filepath.Join(dir, "other")
	input := eofReader{data: []byte("held")}
	output := &lockedBuffer{writeTimes: make(chan time.Time, 1)}
	var diagnostics bytes.Buffer
	status := make(chan int, 1)
	go func() {
		status <- run([]string{"--release-on", "file:" + bad + " && file:" + other}, input, output, &diagnostics)
	}()
	select {
	case <-output.writeTimes:
		t.Fatal("compound file condition released before runtime fatal")
	case <-time.After(50 * time.Millisecond):
	}
	if err := os.Mkdir(bad, 0o700); err != nil {
		t.Fatalf("create fatal directory: %v", err)
	}
	select {
	case got := <-status:
		if got == 0 {
			t.Fatalf("runtime fatal unexpectedly succeeded, diagnostics = %q", diagnostics.String())
		}
	case <-time.After(testTimeout):
		t.Fatal("runtime fatal did not terminate invocation")
	}
	if output.Len() != 0 {
		t.Fatalf("runtime fatal wrote held data: %q", output.String())
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
		status <- run([]string{"--release-on", "file:" + first + " && file:" + second}, input, output, &diagnostics)
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
	args := []string{"--release-on", "file:" + filepath.Join(dir, "first") + " && file:" + filepath.Join(dir, "second")}
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
		"20ms",
		"--release-on",
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
