package main

// This file fixes the file-release parser and monitor contracts.

import (
	"bytes"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestParseConfigAcceptsRepeatableFileConditions(t *testing.T) {
	config, err := parseConfig([]string{
		"--release-on=file:relative/path:with-colon",
		"250ms",
		"--release-on",
		"file:/tmp/ready",
		"--release-on=file::",
	})
	if err != nil {
		t.Fatalf("parseConfig returned error: %v", err)
	}
	if config.delay == nil || *config.delay != 250*time.Millisecond {
		t.Fatalf("delay = %v, want 250ms", config.delay)
	}
	if got, want := config.files, []string{"relative/path:with-colon", "/tmp/ready", ":"}; !equalStrings(got, want) {
		t.Fatalf("files = %q, want %q", got, want)
	}
}

func TestParseConfigRejectsEmptyFilePath(t *testing.T) {
	for _, arg := range []string{"file:"} {
		t.Run(arg, func(t *testing.T) {
			if _, err := parseConfig([]string{"--release-on", arg}); err == nil {
				t.Fatal("parseConfig unexpectedly succeeded")
			}
		})
	}
}

func TestParseConfigAcceptsFileOnlyAndMixedConditions(t *testing.T) {
	for _, args := range [][]string{
		{"--release-on=file:ready"},
		{"--release-on=file:ready", "--release-on=signal:USR1"},
	} {
		if _, err := parseConfig(args); err != nil {
			t.Fatalf("parseConfig(%q) returned error: %v", args, err)
		}
	}
}

func TestParseConfigPreservesDuplicateFileConditions(t *testing.T) {
	config, err := parseConfig([]string{"--release-on=file:ready", "--release-on=file:ready"})
	if err != nil {
		t.Fatalf("parseConfig returned error: %v", err)
	}
	if got, want := config.files, []string{"ready", "ready"}; !equalStrings(got, want) {
		t.Fatalf("files = %q, want %q", got, want)
	}
}

func TestFileProbeClassifiesMissingAndRegularFiles(t *testing.T) {
	dir := t.TempDir()
	missing := filepath.Join(dir, "missing")
	regular := filepath.Join(dir, "ready")
	if err := os.WriteFile(regular, []byte("ready"), 0o600); err != nil {
		t.Fatal(err)
	}

	for _, test := range []struct {
		name      string
		path      string
		wantReady bool
		wantErr   bool
	}{
		{name: "missing", path: missing},
		{name: "regular", path: regular, wantReady: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			ready, err := probeFileRelease(test.path)
			if ready != test.wantReady {
				t.Fatalf("ready = %t, want %t", ready, test.wantReady)
			}
			if (err != nil) != test.wantErr {
				t.Fatalf("error = %v, want error %t", err, test.wantErr)
			}
		})
	}
}

func TestFileProbeRejectsNonRegularFiles(t *testing.T) {
	_, err := probeFileRelease(t.TempDir())
	if err == nil {
		t.Fatal("directory unexpectedly accepted as a release file")
	}
}

func TestFileProbeFollowsSymlinkToRegularFile(t *testing.T) {
	if os.PathSeparator == '\\' {
		t.Skip("symlink permissions are platform dependent on Windows")
	}
	dir := t.TempDir()
	target := filepath.Join(dir, "target")
	link := filepath.Join(dir, "link")
	if err := os.WriteFile(target, []byte("ready"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("create symlink: %v", err)
	}
	ready, err := probeFileRelease(link)
	if err != nil || !ready {
		t.Fatalf("probe symlink = (%t, %v), want (true, nil)", ready, err)
	}
}

func TestFileProbeTreatsDanglingSymlinkAsPending(t *testing.T) {
	if os.PathSeparator == '\\' {
		t.Skip("symlink permissions are platform dependent on Windows")
	}
	dir := t.TempDir()
	link := filepath.Join(dir, "link")
	if err := os.Symlink(filepath.Join(dir, "missing"), link); err != nil {
		t.Skipf("create symlink: %v", err)
	}
	ready, err := probeFileRelease(link)
	if err != nil || ready {
		t.Fatalf("probe dangling symlink = (%t, %v), want (false, nil)", ready, err)
	}
}

func TestFileProbeRejectsSymlinkLoop(t *testing.T) {
	if os.PathSeparator == '\\' {
		t.Skip("symlink permissions are platform dependent on Windows")
	}
	dir := t.TempDir()
	first := filepath.Join(dir, "first")
	second := filepath.Join(dir, "second")
	if err := os.Symlink(second, first); err != nil {
		t.Skipf("create symlink: %v", err)
	}
	if err := os.Symlink(first, second); err != nil {
		t.Skipf("create symlink: %v", err)
	}
	ready, err := probeFileRelease(first)
	if err == nil || ready {
		t.Fatalf("probe symlink loop = (%t, %v), want (false, error)", ready, err)
	}
}

func TestFileOnlyInitialFatalPrecedesImmediateRelease(t *testing.T) {
	input := &trackingReader{}
	var output, diagnostics bytes.Buffer
	status := run([]string{"0s", "--release-on=file:" + t.TempDir()}, input, &output, &diagnostics)
	if status == 0 {
		t.Fatal("directory release condition unexpectedly succeeded")
	}
	if output.Len() != 0 {
		t.Fatalf("fatal initial probe wrote stdout: %q", output.String())
	}
	if diagnostics.Len() == 0 {
		t.Fatal("fatal initial probe produced no diagnostics")
	}
	if input.reads != 0 {
		t.Fatalf("fatal initial probe read stdin %d times", input.reads)
	}
}

func TestFileOnlyEmptyEOFDoesNotWaitForFile(t *testing.T) {
	var output, diagnostics bytes.Buffer
	status := make(chan int, 1)
	go func() {
		status <- run([]string{"--release-on=file:" + filepath.Join(t.TempDir(), "missing")}, strings.NewReader(""), &output, &diagnostics)
	}()
	select {
	case got := <-status:
		if got != 0 {
			t.Fatalf("run status = %d, diagnostics = %q", got, diagnostics.String())
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("empty EOF waited for file release")
	}
}

func TestFileReleaseHoldsDataUntilFileAppears(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ready")
	input := eofReader{data: []byte("held until file")}
	output := &lockedBuffer{writeTimes: make(chan time.Time, 1)}
	var diagnostics bytes.Buffer
	status := make(chan int, 1)
	go func() {
		status <- run([]string{"--release-on=file:" + path}, input, output, &diagnostics)
	}()
	select {
	case <-output.writeTimes:
		t.Fatal("file condition released data before file appeared")
	case <-time.After(50 * time.Millisecond):
	}
	if err := os.WriteFile(path, []byte("ready"), 0o600); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-status:
		if got != 0 {
			t.Fatalf("run status = %d, diagnostics = %q", got, diagnostics.String())
		}
	case <-time.After(testTimeout):
		t.Fatal("file condition did not release held data")
	}
	if got, want := output.String(), "held until file"; got != want {
		t.Fatalf("output = %q, want %q", got, want)
	}
}

func TestFileReleaseFatalWhileClosedSuppressesHeldData(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "ready")
	input := eofReader{data: []byte("must not escape")}
	output := &lockedBuffer{writeTimes: make(chan time.Time, 1)}
	var diagnostics bytes.Buffer
	status := make(chan int, 1)
	go func() {
		status <- run([]string{"--release-on=file:" + path}, input, output, &diagnostics)
	}()
	select {
	case <-output.writeTimes:
		t.Fatal("file condition unexpectedly released data")
	case <-time.After(50 * time.Millisecond):
	}
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-status:
		if got == 0 {
			t.Fatal("fatal file transition unexpectedly succeeded")
		}
	case <-time.After(testTimeout):
		t.Fatal("fatal file transition did not terminate run")
	}
	if output.Len() != 0 {
		t.Fatalf("fatal file transition wrote stdout: %q", output.String())
	}
}

func TestFileMonitorOpensWithoutWaitingForAnotherProbe(t *testing.T) {
	coordinator := newReleaseCoordinator(false)
	slowStarted := make(chan struct{})
	allowSlow := make(chan struct{})
	var calls map[string]int
	var mu sync.Mutex
	calls = make(map[string]int)
	probe := func(path string) (bool, error) {
		mu.Lock()
		calls[path]++
		call := calls[path]
		mu.Unlock()
		if call == 1 {
			return false, nil
		}
		if strings.HasSuffix(path, "slow") {
			select {
			case <-slowStarted:
			default:
				close(slowStarted)
			}
			<-allowSlow
			return false, nil
		}
		<-slowStarted
		return true, nil
	}
	paths := []string{"slow", "fast"}
	monitor, err := newFileMonitorWithProbe(paths, coordinator, probe, time.Millisecond)
	if err != nil {
		t.Fatalf("newFileMonitorWithProbe returned error: %v", err)
	}
	select {
	case <-slowStarted:
	case <-time.After(testTimeout):
		t.Fatal("slow probe did not start")
	}
	select {
	case <-coordinator.release:
	case <-time.After(testTimeout):
		t.Fatal("fast file did not open while slow probe was in flight")
	}
	close(allowSlow)
	coordinator.stopFiles()
	_ = monitor
}

func TestFileMonitorTreatsWrappedNotExistAsPending(t *testing.T) {
	coordinator := newReleaseCoordinator(true)
	probe := func(string) (bool, error) {
		return false, errors.Join(errors.New("probe"), fs.ErrNotExist)
	}
	monitor, err := newFileMonitorWithProbe([]string{"missing"}, coordinator, probe, time.Millisecond)
	if err != nil {
		t.Fatalf("newFileMonitorWithProbe returned error: %v", err)
	}
	defer monitor.Close()
	if err := coordinator.fatalError(); err != nil {
		t.Fatalf("fatalError = %v, want nil", err)
	}
	select {
	case <-coordinator.release:
		t.Fatal("missing file unexpectedly opened the gate")
	default:
	}
}

func TestFileReleaseStopsAfterOpen(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "ready")
	input := strings.NewReader("payload")
	var output, diagnostics bytes.Buffer
	status := make(chan int, 1)
	go func() {
		status <- run([]string{"--release-on=file:" + path}, input, &output, &diagnostics)
	}()

	select {
	case <-status:
		t.Fatal("file-only gate opened before the file existed")
	case <-time.After(30 * time.Millisecond):
	}
	if err := os.WriteFile(path, []byte("ready"), 0o600); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-status:
		if got != 0 {
			t.Fatalf("run status = %d, diagnostics = %q", got, diagnostics.String())
		}
	case <-time.After(testTimeout):
		t.Fatal("file release did not complete")
	}
	if got, want := output.String(), "payload"; got != want {
		t.Fatalf("output = %q, want %q", got, want)
	}
}

func TestReleaseCoordinatorFatalWinsBeforeOpen(t *testing.T) {
	coordinator := newReleaseCoordinator(true)
	fatal := errors.New("probe failed")
	coordinator.reportFatal(fatal)
	if err := coordinator.requestOpen(); !errors.Is(err, fatal) {
		t.Fatalf("requestOpen error = %v, want %v", err, fatal)
	}
	select {
	case <-coordinator.release:
		t.Fatal("fatal coordinator opened the gate")
	default:
	}
}

func TestReleaseCoordinatorIgnoresFatalAfterOpen(t *testing.T) {
	coordinator := newReleaseCoordinator(false)
	if err := coordinator.requestOpen(); err != nil {
		t.Fatalf("requestOpen returned error: %v", err)
	}
	fatal := errors.New("late probe failed")
	coordinator.reportFatal(fatal)
	if got := coordinator.fatalError(); got != nil {
		t.Fatalf("fatalError = %v, want nil", got)
	}
	select {
	case <-coordinator.release:
	default:
		t.Fatal("coordinator did not open the gate")
	}
}

func TestReleaseCoordinatorEmptyCompletionStopsAndWinsOverLateFatal(t *testing.T) {
	coordinator := newReleaseCoordinator(false)
	if err := coordinator.completeEmpty(); err != nil {
		t.Fatalf("completeEmpty returned error: %v", err)
	}
	coordinator.reportFatal(errors.New("late probe failed"))
	if got := coordinator.fatalError(); got != nil {
		t.Fatalf("fatalError = %v, want nil", got)
	}
	select {
	case <-coordinator.files:
	default:
		t.Fatal("empty completion did not stop file monitoring")
	}
	select {
	case <-coordinator.release:
		t.Fatal("empty completion opened the gate")
	default:
	}
}

func TestFileMonitorSkipsProbeWhenStopArrivesAfterTick(t *testing.T) {
	coordinator := newReleaseCoordinator(false)
	ticks := make(chan time.Time, 1)
	probeCalled := make(chan struct{}, 1)
	monitor := &fileMonitor{
		coordinator: coordinator,
		probe: func(string) (bool, error) {
			probeCalled <- struct{}{}
			return false, nil
		},
	}
	done := make(chan struct{})
	go func() {
		monitor.watchPathWithTicks("ready", ticks, func() {
			coordinator.stopFiles()
		})
		close(done)
	}()
	ticks <- time.Now()

	select {
	case <-done:
	case <-time.After(testTimeout):
		t.Fatal("watcher did not stop after the tick")
	}
	select {
	case <-probeCalled:
		t.Fatal("watcher started a probe after file monitoring stopped")
	default:
	}
}

func TestReleaseCoordinatorPreventsProbeReservationAfterStop(t *testing.T) {
	coordinator := newReleaseCoordinator(false)
	coordinator.stopFiles()
	if coordinator.beginFileProbe() {
		t.Fatal("stopped coordinator reserved a new file probe")
	}
}

func TestInitialRegularAndFatalProbeFatalWins(t *testing.T) {
	coordinator := newReleaseCoordinator(true)
	fatal := errors.New("initial fatal")
	var mu sync.Mutex
	probed := make(map[string]int)
	probe := func(path string) (bool, error) {
		mu.Lock()
		probed[path]++
		mu.Unlock()
		if path == "regular" {
			return true, nil
		}
		return false, fatal
	}
	monitor, err := newFileMonitorWithProbe([]string{"regular", "fatal"}, coordinator, probe, time.Millisecond)
	if err == nil || !errors.Is(err, fatal) {
		t.Fatalf("newFileMonitorWithProbe error = %v, want %v", err, fatal)
	}
	if monitor == nil {
		t.Fatal("newFileMonitorWithProbe returned nil monitor")
	}
	select {
	case <-coordinator.release:
		t.Fatal("regular initial result opened gate despite fatal result")
	default:
	}
	mu.Lock()
	defer mu.Unlock()
	for _, path := range []string{"regular", "fatal"} {
		if got := probed[path]; got != 1 {
			t.Fatalf("probe count for %q = %d, want 1", path, got)
		}
	}
}

func TestInitialPendingOpenAndFatalProbeFatalWins(t *testing.T) {
	coordinator := newReleaseCoordinator(true)
	fatal := errors.New("fatal after pending signal")
	probe := func(string) (bool, error) {
		if err := coordinator.requestOpen(); err != nil {
			return false, err
		}
		return false, fatal
	}
	monitor, err := newFileMonitorWithProbe([]string{"fatal"}, coordinator, probe, time.Millisecond)
	if err == nil || !errors.Is(err, fatal) {
		t.Fatalf("newFileMonitorWithProbe error = %v, want %v", err, fatal)
	}
	if monitor == nil {
		t.Fatal("newFileMonitorWithProbe returned nil monitor")
	}
	select {
	case <-coordinator.release:
		t.Fatal("pending open opened gate despite fatal initial probe")
	default:
	}
}

func TestInitialPendingOpenDoesNotSpawnStoppedWatchers(t *testing.T) {
	coordinator := newReleaseCoordinator(true)
	var mu sync.Mutex
	calls := 0
	probe := func(string) (bool, error) {
		mu.Lock()
		calls++
		mu.Unlock()
		if err := coordinator.requestOpen(); err != nil {
			return false, err
		}
		return false, nil
	}
	monitor, err := newFileMonitorWithProbe([]string{"pending"}, coordinator, probe, time.Millisecond)
	if err != nil {
		t.Fatalf("newFileMonitorWithProbe returned error: %v", err)
	}
	defer monitor.Close()
	time.Sleep(20 * time.Millisecond)
	mu.Lock()
	defer mu.Unlock()
	if calls != 1 {
		t.Fatalf("probe calls = %d, want only the initial probe", calls)
	}
}

func TestInitialRegularFileOpensBeforeFirstStdinRead(t *testing.T) {
	coordinator := newReleaseCoordinator(true)
	monitor, err := newFileMonitorWithProbe([]string{"ready"}, coordinator, func(string) (bool, error) {
		return true, nil
	}, time.Millisecond)
	if err != nil {
		t.Fatalf("newFileMonitorWithProbe returned error: %v", err)
	}
	defer monitor.Close()
	readStartedAfterOpen := false
	input := readerFunc(func([]byte) (int, error) {
		select {
		case <-coordinator.release:
			readStartedAfterOpen = true
		default:
		}
		return 0, io.EOF
	})
	if err := forward(input, &bytes.Buffer{}, nil, coordinator.release); err != nil {
		t.Fatalf("forward returned error: %v", err)
	}
	if !readStartedAfterOpen {
		t.Fatal("first stdin read started before initial regular file opened the gate")
	}
}

func TestCollectInitialFileProbeResultsUsesConfiguredPathOrder(t *testing.T) {
	firstFatal := errors.New("first fatal")
	secondFatal := errors.New("second fatal")
	results := make(chan fileProbeResult, 2)
	results <- fileProbeResult{index: 1, err: secondFatal}
	results <- fileProbeResult{index: 0, err: firstFatal}

	first, anyReady := collectInitialFileProbeResults(results, 2)
	if !errors.Is(first, firstFatal) {
		t.Fatalf("initial fatal = %v, want configured-first error %v", first, firstFatal)
	}
	if errors.Is(first, secondFatal) {
		t.Fatalf("initial fatal = %v, selected later configured path", first)
	}
	if anyReady {
		t.Fatal("initial results reported a ready file without a ready result")
	}
}

func TestInitialFileProbesStartInParallel(t *testing.T) {
	coordinator := newReleaseCoordinator(true)
	paths := []string{"first", "second", "third"}
	started := make(chan string, len(paths))
	allowProbes := make(chan struct{})
	var unblockOnce sync.Once
	unblock := func() {
		unblockOnce.Do(func() { close(allowProbes) })
	}
	t.Cleanup(func() {
		unblock()
		coordinator.stopFiles()
	})

	probe := func(path string) (bool, error) {
		started <- path
		<-allowProbes
		return false, nil
	}
	result := make(chan error, 1)
	go func() {
		_, err := newFileMonitorWithProbe(paths, coordinator, probe, time.Millisecond)
		result <- err
	}()
	for range paths {
		select {
		case <-started:
		case <-time.After(testTimeout):
			t.Fatal("initial file probes did not all start before the release barrier")
		}
	}
	coordinator.stopFiles()
	unblock()
	select {
	case err := <-result:
		if err != nil {
			t.Fatalf("newFileMonitorWithProbe returned error: %v", err)
		}
	case <-time.After(testTimeout):
		t.Fatal("initial file probes did not complete after the release barrier")
	}
}

func TestForwardDelayedReportsClosedFailureChannel(t *testing.T) {
	started := make(chan struct{})
	allowRead := make(chan struct{})
	var unblockOnce sync.Once
	unblock := func() {
		unblockOnce.Do(func() { close(allowRead) })
	}
	t.Cleanup(unblock)
	input := readerFunc(func([]byte) (int, error) {
		close(started)
		<-allowRead
		return 1, nil
	})
	failures := make(chan error)
	var output bytes.Buffer
	status := make(chan error, 1)
	held := []byte("heldx")
	go func() {
		status <- forwardDelayedWithFailure(input, &output, nil, nil, failures, nil, nil, held, len("held"))
	}()
	select {
	case <-started:
	case <-time.After(testTimeout):
		t.Fatal("blocking reader did not start")
	}
	close(failures)
	var err error
	select {
	case err = <-status:
	case <-time.After(testTimeout):
		t.Fatal("forwardDelayed did not report the closed failure channel")
	}
	if err == nil {
		t.Fatal("closed failure channel returned silent success")
	}
	if !errors.Is(err, errReleaseFailureChannelClosed) {
		t.Fatalf("closed failure channel error = %v, want %v", err, errReleaseFailureChannelClosed)
	}
	if output.Len() != 0 {
		t.Fatalf("closed failure channel wrote held data: %q", output.String())
	}
}

func equalStrings(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for index := range got {
		if got[index] != want[index] {
			return false
		}
	}
	return true
}

type readerFunc func([]byte) (int, error)

func (read readerFunc) Read(buffer []byte) (int, error) {
	return read(buffer)
}
