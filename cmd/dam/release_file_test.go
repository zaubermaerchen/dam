package main

// This file fixes the file-release parser and monitor contracts.

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
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

func TestNextFilePollIntervalDoublesAndCaps(t *testing.T) {
	interval := filePollInterval
	for _, want := range []time.Duration{
		20 * time.Millisecond,
		40 * time.Millisecond,
		80 * time.Millisecond,
		160 * time.Millisecond,
		250 * time.Millisecond,
		250 * time.Millisecond,
	} {
		interval = nextFilePollInterval(interval)
		if interval != want {
			t.Fatalf("nextFilePollInterval = %v, want %v", interval, want)
		}
	}
}

func TestFileMonitorUsesAdaptiveBackoffAfterPendingProbe(t *testing.T) {
	coordinator := newReleaseCoordinator(false)
	wantDelays := []time.Duration{
		10 * time.Millisecond,
		20 * time.Millisecond,
		40 * time.Millisecond,
		80 * time.Millisecond,
		160 * time.Millisecond,
		250 * time.Millisecond,
		250 * time.Millisecond,
	}
	var (
		mu     sync.Mutex
		delays []time.Duration
	)
	done := make(chan struct{})
	monitor := &fileMonitor{
		coordinator: coordinator,
		probe: func(string) (bool, error) {
			return false, nil
		},
		interval: filePollInterval,
		wait: func(_ string, _ <-chan struct{}, delay time.Duration) bool {
			mu.Lock()
			delays = append(delays, delay)
			last := len(delays) == len(wantDelays)
			mu.Unlock()
			if last {
				coordinator.stopFiles()
				return false
			}
			return true
		},
	}
	go func() {
		monitor.watchPath("missing")
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(testTimeout):
		t.Fatal("adaptive file watcher did not stop")
	}
	mu.Lock()
	got := slices.Clone(delays)
	mu.Unlock()
	if !equalDurations(got, wantDelays) {
		t.Fatalf("poll delays = %v, want %v", got, wantDelays)
	}
}

func TestFileMonitorTreatsWrappedNotExistDuringPollingAsPending(t *testing.T) {
	coordinator := newReleaseCoordinator(false)
	t.Cleanup(coordinator.stopFiles)
	waits := make(chan time.Duration, 2)
	calls := 0
	monitor := &fileMonitor{
		coordinator: coordinator,
		interval:    filePollInterval,
		wait: func(_ string, stop <-chan struct{}, delay time.Duration) bool {
			select {
			case waits <- delay:
			case <-stop:
				return false
			}
			if delay == 2*filePollInterval {
				coordinator.stopFiles()
				return false
			}
			return true
		},
		probe: func(string) (bool, error) {
			calls++
			return false, fmt.Errorf("probe missing: %w", fs.ErrNotExist)
		},
	}
	done := make(chan struct{})
	go func() {
		monitor.watchPath("missing")
		close(done)
	}()
	select {
	case delay := <-waits:
		if delay != filePollInterval {
			t.Fatalf("first poll delay = %v, want %v", delay, filePollInterval)
		}
	case <-time.After(testTimeout):
		t.Fatal("first poll was not scheduled")
	}
	select {
	case delay := <-waits:
		if delay != 2*filePollInterval {
			t.Fatalf("wrapped not-exist next poll delay = %v, want %v", delay, 2*filePollInterval)
		}
	case <-time.After(testTimeout):
		t.Fatal("wrapped not-exist did not schedule a next poll")
	}
	select {
	case <-done:
	case <-time.After(testTimeout):
		t.Fatal("file watcher did not stop")
	}
	if calls != 1 {
		t.Fatalf("probe calls = %d, want 1", calls)
	}
	if err := coordinator.fatalError(); err != nil {
		t.Fatalf("fatalError = %v, want nil", err)
	}
}

func TestFileMonitorWaitsForProbeCompletionBeforeNextBackoff(t *testing.T) {
	coordinator := newReleaseCoordinator(false)
	probeStarted := make(chan struct{})
	allowProbe := make(chan struct{})
	waitCalls := make(chan time.Duration, 2)
	monitor := &fileMonitor{
		coordinator: coordinator,
		interval:    filePollInterval,
		wait: func(_ string, _ <-chan struct{}, delay time.Duration) bool {
			waitCalls <- delay
			if delay == 2*filePollInterval {
				coordinator.stopFiles()
				return false
			}
			return true
		},
		probe: func(string) (bool, error) {
			close(probeStarted)
			<-allowProbe
			return false, nil
		},
	}
	done := make(chan struct{})
	go func() {
		monitor.watchPath("missing")
		close(done)
	}()
	select {
	case delay := <-waitCalls:
		if delay != filePollInterval {
			t.Fatalf("first poll delay = %v, want %v", delay, filePollInterval)
		}
	case <-time.After(testTimeout):
		t.Fatal("first poll was not scheduled")
	}
	select {
	case <-probeStarted:
	case <-time.After(testTimeout):
		t.Fatal("probe did not start")
	}
	select {
	case delay := <-waitCalls:
		t.Fatalf("next poll was scheduled before probe completed, delay %v", delay)
	default:
	}
	close(allowProbe)
	select {
	case delay := <-waitCalls:
		if delay != 2*filePollInterval {
			t.Fatalf("second poll delay = %v, want %v", delay, 2*filePollInterval)
		}
	case <-time.After(testTimeout):
		t.Fatal("second poll was not scheduled after probe completion")
	}
	select {
	case <-done:
	case <-time.After(testTimeout):
		t.Fatal("file watcher did not stop")
	}
}

func TestFileMonitorStopsWhileBackedOff(t *testing.T) {
	for _, test := range []struct {
		name string
		stop func(*releaseCoordinator, error)
	}{
		{
			name: "open",
			stop: func(coordinator *releaseCoordinator, _ error) {
				if err := coordinator.requestOpen(); err != nil {
					panic(err)
				}
			},
		},
		{
			name: "fatal",
			stop: func(coordinator *releaseCoordinator, fatal error) {
				coordinator.reportFatal(fatal)
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			coordinator := newReleaseCoordinator(false)
			backedOff := make(chan struct{})
			var waits int
			var mu sync.Mutex
			fatal := errors.New("backoff fatal")
			monitor := &fileMonitor{
				coordinator: coordinator,
				interval:    filePollInterval,
				wait: func(_ string, stop <-chan struct{}, _ time.Duration) bool {
					mu.Lock()
					waits++
					current := waits
					mu.Unlock()
					if current == 2 {
						close(backedOff)
						<-stop
						return false
					}
					return true
				},
				probe: func(string) (bool, error) {
					return false, nil
				},
			}
			done := make(chan struct{})
			go func() {
				monitor.watchPath("missing")
				close(done)
			}()
			select {
			case <-backedOff:
			case <-time.After(testTimeout):
				t.Fatal("file watcher did not enter backoff")
			}
			test.stop(coordinator, fatal)
			select {
			case <-done:
			case <-time.After(testTimeout):
				t.Fatal("file watcher did not stop after coordinator transition")
			}
			if test.name == "fatal" && !errors.Is(coordinator.fatalError(), fatal) {
				t.Fatalf("fatalError = %v, want %v", coordinator.fatalError(), fatal)
			}
		})
	}
}

func TestFileMonitorIgnoresInFlightProbeAfterStop(t *testing.T) {
	coordinator := newReleaseCoordinator(false)
	probeStarted := make(chan struct{})
	allowProbe := make(chan struct{})
	monitor := &fileMonitor{
		coordinator: coordinator,
		interval:    filePollInterval,
		wait: func(_ string, _ <-chan struct{}, _ time.Duration) bool {
			return true
		},
		probe: func(string) (bool, error) {
			close(probeStarted)
			<-allowProbe
			return true, nil
		},
	}
	done := make(chan struct{})
	go func() {
		monitor.watchPath("ready")
		close(done)
	}()
	select {
	case <-probeStarted:
	case <-time.After(testTimeout):
		t.Fatal("probe did not start")
	}
	if err := coordinator.completeEmpty(); err != nil {
		t.Fatalf("completeEmpty returned error: %v", err)
	}
	close(allowProbe)
	select {
	case <-done:
	case <-time.After(testTimeout):
		t.Fatal("file watcher did not stop after in-flight probe")
	}
	select {
	case <-coordinator.release:
		t.Fatal("in-flight ready result opened the stopped coordinator")
	default:
	}
	if err := coordinator.fatalError(); err != nil {
		t.Fatalf("in-flight probe reported fatal error: %v", err)
	}
}

func TestFileMonitorMaintainsIndependentBackoffPerPath(t *testing.T) {
	coordinator := newReleaseCoordinator(false)
	t.Cleanup(coordinator.stopFiles)
	want := []time.Duration{filePollInterval, 2 * filePollInterval}
	type pollRequest struct {
		path  string
		delay time.Duration
		allow chan bool
	}
	paths := []string{"first", "second"}
	requests := make(chan pollRequest, len(paths)*len(want))
	monitor := &fileMonitor{
		coordinator: coordinator,
		interval:    filePollInterval,
		wait: func(path string, stop <-chan struct{}, delay time.Duration) bool {
			allow := make(chan bool)
			select {
			case requests <- pollRequest{path: path, delay: delay, allow: allow}:
			case <-stop:
				return false
			}
			select {
			case allowed := <-allow:
				return allowed
			case <-stop:
				return false
			}
		},
		probe: func(string) (bool, error) {
			return false, nil
		},
	}
	var wg sync.WaitGroup
	for _, path := range paths {
		wg.Add(1)
		go func() {
			defer wg.Done()
			monitor.watchPath(path)
		}()
	}
	delays := make(map[string][]time.Duration)
	for range 4 {
		var request pollRequest
		select {
		case request = <-requests:
		case <-time.After(testTimeout):
			t.Fatal("timed out waiting for independent poll request")
		}
		pathDelays := delays[request.path]
		index := len(pathDelays)
		if index >= len(want) {
			t.Fatalf("received too many poll requests for %q", request.path)
		}
		if request.delay != want[index] {
			t.Fatalf("poll delay for %q = %v, want %v", request.path, request.delay, want[index])
		}
		delays[request.path] = append(pathDelays, request.delay)
		request.allow <- index == 0
	}
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(testTimeout):
		t.Fatal("file watchers did not stop")
	}
	for _, path := range paths {
		if !equalDurations(delays[path], want) {
			t.Fatalf("poll delays for %q = %v, want %v", path, delays[path], want)
		}
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

func equalDurations(got, want []time.Duration) bool {
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
