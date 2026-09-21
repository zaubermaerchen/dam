package condition

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"sync"
	"testing"
	"time"
)

const fileMonitorTestTimeout = time.Second

func TestProbeFileReleaseClassifiesMissingAndRegularFiles(t *testing.T) {
	dir := t.TempDir()
	regular := filepath.Join(dir, "ready")
	if err := os.WriteFile(regular, []byte("ready"), 0o600); err != nil {
		t.Fatal(err)
	}

	for _, test := range []struct {
		name string
		path string
		want bool
	}{
		{name: "missing", path: filepath.Join(dir, "missing")},
		{name: "regular", path: regular, want: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			ready, err := probeFileRelease(test.path)
			if err != nil {
				t.Fatalf("probe error = %v, want nil", err)
			}
			if ready != test.want {
				t.Fatalf("ready = %t, want %t", ready, test.want)
			}
		})
	}
}

func TestProbeFileReleaseTreatsNonRegularFilesAsPending(t *testing.T) {
	ready, err := probeFileRelease(t.TempDir())
	if err != nil || ready {
		t.Fatalf("directory probe = (%t, %v), want (false, nil)", ready, err)
	}
}

func TestProbeFileReleaseTreatsFIFOAsPending(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("named pipes are not created with mkfifo on Windows")
	}
	path := filepath.Join(t.TempDir(), "ready")
	if err := exec.Command("mkfifo", path).Run(); err != nil {
		t.Skipf("create FIFO: %v", err)
	}
	ready, err := probeFileRelease(path)
	if err != nil || ready {
		t.Fatalf("FIFO probe = (%t, %v), want (false, nil)", ready, err)
	}
}

func TestProbeFileReleaseFollowsSymlinkToRegularFile(t *testing.T) {
	if runtime.GOOS == "windows" {
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

func TestProbeFileReleaseTreatsDanglingSymlinkAsPending(t *testing.T) {
	if runtime.GOOS == "windows" {
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

func TestProbeFileReleaseTreatsSymlinkLoopAndStatErrorsAsPending(t *testing.T) {
	dir := t.TempDir()
	parent := filepath.Join(dir, "parent")
	if err := os.WriteFile(parent, []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name string
		path string
	}{
		{name: "not a directory", path: filepath.Join(parent, "ready")},
	}
	if runtime.GOOS != "windows" {
		first := filepath.Join(dir, "first")
		second := filepath.Join(dir, "second")
		if err := os.Symlink(second, first); err != nil {
			t.Skipf("create first symlink: %v", err)
		}
		if err := os.Symlink(first, second); err != nil {
			t.Skipf("create second symlink: %v", err)
		}
		tests = append(tests, struct {
			name string
			path string
		}{name: "symlink loop", path: first})
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			ready, err := probeFileRelease(test.path)
			if err != nil || ready {
				t.Fatalf("probe = (%t, %v), want (false, nil)", ready, err)
			}
		})
	}
}

func TestFileMonitorRetriesProbeErrors(t *testing.T) {
	engine := newEngineWithGroups(true, []Group{{Members: []Condition{{Kind: "file", Source: "ready"}}}})
	defer engine.Close()

	var mu sync.Mutex
	calls := 0
	probe := func(string) (bool, error) {
		mu.Lock()
		calls++
		call := calls
		mu.Unlock()
		if call == 1 {
			return false, errors.New("transient stat failure")
		}
		return true, nil
	}
	monitor, err := newFileMonitorWithProbe([]string{"ready"}, engine, probe, time.Millisecond)
	if err != nil {
		t.Fatalf("newFileMonitorWithProbe returned error: %v", err)
	}
	defer monitor.Close()

	select {
	case <-engine.Satisfied():
	case <-time.After(fileMonitorTestTimeout):
		t.Fatal("file monitor did not retry a failed probe")
	}
	mu.Lock()
	gotCalls := calls
	mu.Unlock()
	if gotCalls < 2 {
		t.Fatalf("probe calls = %d, want at least 2", gotCalls)
	}
	if err := engine.fatalError(); err != nil {
		t.Fatalf("fatalError = %v, want nil", err)
	}
}

func TestFileMonitorOpensWithoutWaitingForAnotherProbe(t *testing.T) {
	engine := newEngine(false)
	defer engine.Close()
	slowStarted := make(chan struct{})
	allowSlow := make(chan struct{})
	defer close(allowSlow)

	var (
		mu    sync.Mutex
		calls = make(map[string]int)
	)
	probe := func(path string) (bool, error) {
		mu.Lock()
		calls[path]++
		call := calls[path]
		mu.Unlock()
		if call == 1 {
			return false, nil
		}
		if path == "slow" {
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
	monitor, err := newFileMonitorWithProbe([]string{"slow", "fast"}, engine, probe, time.Millisecond)
	if err != nil {
		t.Fatalf("newFileMonitorWithProbe returned error: %v", err)
	}
	defer monitor.Close()

	select {
	case <-slowStarted:
	case <-time.After(fileMonitorTestTimeout):
		t.Fatal("slow probe did not start")
	}
	select {
	case <-engine.Satisfied():
	case <-time.After(fileMonitorTestTimeout):
		t.Fatal("fast file did not open while slow probe was in flight")
	}
}

func TestFileMonitorTreatsWrappedStatErrorsAsPending(t *testing.T) {
	engine := newEngineWithGroups(true, []Group{{Members: []Condition{{Kind: "file", Source: "missing"}}}})
	defer engine.Close()
	probe := func(string) (bool, error) {
		return false, errors.Join(errors.New("probe"), fs.ErrNotExist)
	}
	monitor, err := newFileMonitorWithProbe([]string{"missing"}, engine, probe, time.Millisecond)
	if err != nil {
		t.Fatalf("newFileMonitorWithProbe returned error: %v", err)
	}
	defer monitor.Close()

	select {
	case <-engine.Satisfied():
		t.Fatal("missing file unexpectedly opened the gate")
	case <-time.After(20 * time.Millisecond):
	}
	select {
	case err := <-engine.Failures():
		t.Fatalf("wrapped not-exist error became fatal: %v", err)
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
	engine := newEngine(false)
	defer engine.Close()
	want := []time.Duration{
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
		engine:   engine,
		probe:    func(string) (bool, error) { return false, nil },
		interval: filePollInterval,
		wait: func(_ string, _ <-chan struct{}, delay time.Duration) bool {
			mu.Lock()
			delays = append(delays, delay)
			last := len(delays) == len(want)
			mu.Unlock()
			if last {
				engine.stopFiles()
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
	case <-time.After(fileMonitorTestTimeout):
		t.Fatal("adaptive file watcher did not stop")
	}
	mu.Lock()
	got := slices.Clone(delays)
	mu.Unlock()
	if !slices.Equal(got, want) {
		t.Fatalf("poll delays = %v, want %v", got, want)
	}
}

func TestFileMonitorTreatsWrappedStatErrorsDuringPollingAsPending(t *testing.T) {
	engine := newEngine(false)
	defer engine.Close()
	waits := make(chan time.Duration, 2)
	calls := 0
	monitor := &fileMonitor{
		engine:   engine,
		interval: filePollInterval,
		wait: func(_ string, stop <-chan struct{}, delay time.Duration) bool {
			select {
			case waits <- delay:
			case <-stop:
				return false
			}
			if delay == 2*filePollInterval {
				engine.stopFiles()
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
	case <-time.After(fileMonitorTestTimeout):
		t.Fatal("first poll was not scheduled")
	}
	select {
	case delay := <-waits:
		if delay != 2*filePollInterval {
			t.Fatalf("wrapped not-exist next poll delay = %v, want %v", delay, 2*filePollInterval)
		}
	case <-time.After(fileMonitorTestTimeout):
		t.Fatal("wrapped not-exist did not schedule a next poll")
	}
	select {
	case <-done:
	case <-time.After(fileMonitorTestTimeout):
		t.Fatal("file watcher did not stop")
	}
	if calls != 1 {
		t.Fatalf("probe calls = %d, want 1", calls)
	}
	if err := engine.fatalError(); err != nil {
		t.Fatalf("fatalError = %v, want nil", err)
	}
}

func TestFileMonitorWaitsForProbeCompletionBeforeNextBackoff(t *testing.T) {
	engine := newEngine(false)
	defer engine.Close()
	probeStarted := make(chan struct{})
	allowProbe := make(chan struct{})
	waitCalls := make(chan time.Duration, 2)
	monitor := &fileMonitor{
		engine:   engine,
		interval: filePollInterval,
		wait: func(_ string, _ <-chan struct{}, delay time.Duration) bool {
			waitCalls <- delay
			if delay == 2*filePollInterval {
				engine.stopFiles()
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
	case <-time.After(fileMonitorTestTimeout):
		t.Fatal("first poll was not scheduled")
	}
	select {
	case <-probeStarted:
	case <-time.After(fileMonitorTestTimeout):
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
	case <-time.After(fileMonitorTestTimeout):
		t.Fatal("second poll was not scheduled after probe completion")
	}
	select {
	case <-done:
	case <-time.After(fileMonitorTestTimeout):
		t.Fatal("file watcher did not stop")
	}
}

func TestFileMonitorMaintainsIndependentBackoffPerPath(t *testing.T) {
	engine := newEngine(false)
	defer engine.Close()
	want := []time.Duration{filePollInterval, 2 * filePollInterval}
	type pollRequest struct {
		path  string
		delay time.Duration
		allow chan bool
	}
	paths := []string{"first", "second"}
	requests := make(chan pollRequest, len(paths)*len(want))
	monitor := &fileMonitor{
		engine:   engine,
		interval: filePollInterval,
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
		probe: func(string) (bool, error) { return false, nil },
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
		case <-time.After(fileMonitorTestTimeout):
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
	case <-time.After(fileMonitorTestTimeout):
		t.Fatal("file watchers did not stop")
	}
	for _, path := range paths {
		if !slices.Equal(delays[path], want) {
			t.Fatalf("poll delays for %q = %v, want %v", path, delays[path], want)
		}
	}
}

func TestFileMonitorStopsWhileBackedOff(t *testing.T) {
	for _, test := range []struct {
		name string
		stop func(*Engine, error)
	}{
		{
			name: "open",
			stop: func(engine *Engine, _ error) {
				if err := engine.satisfyCondition("file", "missing"); err != nil {
					panic(err)
				}
			},
		},
		{
			name: "fatal",
			stop: func(engine *Engine, fatal error) {
				engine.reportFatal(fatal)
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			engine := newEngine(false)
			defer engine.Close()
			backedOff := make(chan struct{})
			var (
				mu    sync.Mutex
				waits int
			)
			fatal := errors.New("backoff fatal")
			monitor := &fileMonitor{
				engine:   engine,
				interval: filePollInterval,
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
				probe: func(string) (bool, error) { return false, nil },
			}
			done := make(chan struct{})
			go func() {
				monitor.watchPath("missing")
				close(done)
			}()
			select {
			case <-backedOff:
			case <-time.After(fileMonitorTestTimeout):
				t.Fatal("file watcher did not enter backoff")
			}
			test.stop(engine, fatal)
			select {
			case <-done:
			case <-time.After(fileMonitorTestTimeout):
				t.Fatal("file watcher did not stop after coordinator transition")
			}
			if test.name == "fatal" && !errors.Is(engine.fatalError(), fatal) {
				t.Fatalf("fatalError = %v, want %v", engine.fatalError(), fatal)
			}
		})
	}
}

func TestFileMonitorIgnoresInFlightProbeAfterStop(t *testing.T) {
	engine := newEngine(false)
	defer engine.Close()
	probeStarted := make(chan struct{})
	allowProbe := make(chan struct{})
	monitor := &fileMonitor{
		engine:   engine,
		interval: filePollInterval,
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
	case <-time.After(fileMonitorTestTimeout):
		t.Fatal("probe did not start")
	}
	selected, err := engine.CompleteEmpty()
	if err != nil {
		t.Fatal(err)
	}
	if selected {
		t.Fatal("empty completion reported a prior selection")
	}
	close(allowProbe)
	select {
	case <-done:
	case <-time.After(fileMonitorTestTimeout):
		t.Fatal("file watcher did not stop after in-flight probe")
	}
	select {
	case <-engine.Satisfied():
		t.Fatal("in-flight ready result opened the stopped engine")
	default:
	}
	if err := engine.fatalError(); err != nil {
		t.Fatalf("in-flight probe reported fatal error: %v", err)
	}
}

func TestFileMonitorSkipsProbeWhenStopArrivesAfterPollWait(t *testing.T) {
	engine := newEngine(false)
	defer engine.Close()
	probeCalled := make(chan struct{}, 1)
	monitor := &fileMonitor{
		engine:   engine,
		interval: filePollInterval,
		wait: func(_ string, _ <-chan struct{}, _ time.Duration) bool {
			engine.stopFiles()
			return true
		},
		probe: func(string) (bool, error) {
			probeCalled <- struct{}{}
			return false, nil
		},
	}
	done := make(chan struct{})
	go func() {
		monitor.watchPath("ready")
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(fileMonitorTestTimeout):
		t.Fatal("watcher did not stop after the tick")
	}
	select {
	case <-probeCalled:
		t.Fatal("watcher started a probe after file monitoring stopped")
	default:
	}
}

func TestEngineFatalAndSelectionPrecedence(t *testing.T) {
	t.Run("fatal before empty completion", func(t *testing.T) {
		engine := newEngine(false)
		defer engine.Close()
		fatal := errors.New("probe failed")
		engine.reportFatal(fatal)
		selected, err := engine.CompleteEmpty()
		if selected {
			t.Fatal("fatal engine reported root selection")
		}
		if !errors.Is(err, fatal) {
			t.Fatalf("empty completion error = %v, want %v", err, fatal)
		}
	})

	t.Run("fatal before selection", func(t *testing.T) {
		engine := newEngine(false)
		defer engine.Close()
		fatal := errors.New("probe failed")
		engine.reportFatal(fatal)
		if err := engine.satisfyCondition("file", "ready"); !errors.Is(err, fatal) {
			t.Fatalf("selection error = %v, want %v", err, fatal)
		}
		select {
		case <-engine.Satisfied():
			t.Fatal("fatal engine opened the gate")
		default:
		}
	})

	t.Run("selection before fatal", func(t *testing.T) {
		engine := newEngine(false)
		defer engine.Close()
		if err := engine.satisfyCondition("file", "ready"); err != nil {
			t.Fatal(err)
		}
		engine.reportFatal(errors.New("late probe failed"))
		if err := engine.fatalError(); err != nil {
			t.Fatalf("fatalError = %v, want nil", err)
		}
		select {
		case <-engine.Satisfied():
		default:
			t.Fatal("engine did not open the gate")
		}
	})

	t.Run("empty completion before fatal", func(t *testing.T) {
		engine := newEngine(false)
		defer engine.Close()
		selected, err := engine.CompleteEmpty()
		if err != nil {
			t.Fatal(err)
		}
		if selected {
			t.Fatal("empty completion reported a prior selection")
		}
		engine.reportFatal(errors.New("late probe failed"))
		if err := engine.fatalError(); err != nil {
			t.Fatalf("fatalError = %v, want nil", err)
		}
		select {
		case <-engine.Satisfied():
			t.Fatal("empty completion opened the gate")
		default:
		}
	})
}

func TestEnginePreventsProbeReservationAfterStop(t *testing.T) {
	engine := newEngine(false)
	defer engine.Close()
	engine.stopFiles()
	if engine.beginFileProbe() {
		t.Fatal("stopped engine reserved a new file probe")
	}
}

func TestInitialRegularAndProbeErrorRemainsPending(t *testing.T) {
	engine := newEngine(true)
	defer engine.Close()
	var mu sync.Mutex
	probed := make(map[string]int)
	probe := func(path string) (bool, error) {
		mu.Lock()
		probed[path]++
		mu.Unlock()
		if path == "regular" {
			return true, nil
		}
		return false, errors.New("transient initial probe failure")
	}
	monitor, err := newFileMonitorWithProbe([]string{"regular", "error"}, engine, probe, time.Millisecond)
	if err != nil {
		t.Fatalf("newFileMonitorWithProbe returned error: %v", err)
	}
	defer monitor.Close()
	select {
	case <-engine.Satisfied():
	default:
		t.Fatal("regular initial result did not open gate despite pending probe error")
	}
	mu.Lock()
	defer mu.Unlock()
	for _, path := range []string{"regular", "error"} {
		if got := probed[path]; got != 1 {
			t.Fatalf("probe count for %q = %d, want 1", path, got)
		}
	}
}

func TestInitialPendingSelectionDoesNotSpawnStoppedWatchers(t *testing.T) {
	engine := newEngine(true)
	defer engine.Close()
	var mu sync.Mutex
	calls := 0
	probe := func(string) (bool, error) {
		mu.Lock()
		calls++
		mu.Unlock()
		if err := engine.satisfyCondition("file", "pending"); err != nil {
			return false, err
		}
		return false, errors.New("transient initial probe failure")
	}
	monitor, err := newFileMonitorWithProbe([]string{"pending"}, engine, probe, time.Millisecond)
	if err != nil {
		t.Fatalf("newFileMonitorWithProbe returned error: %v", err)
	}
	defer monitor.Close()
	select {
	case <-engine.Satisfied():
	default:
		t.Fatal("pending initial selection did not open gate")
	}
	time.Sleep(20 * time.Millisecond)
	mu.Lock()
	gotCalls := calls
	mu.Unlock()
	if gotCalls != 1 {
		t.Fatalf("probe calls = %d, want only the initial probe", gotCalls)
	}
}

func TestCollectInitialFileProbeResultsIgnoresProbeErrors(t *testing.T) {
	results := make(chan fileProbeResult, 2)
	results <- fileProbeResult{index: 1, err: errors.New("second probe error")}
	results <- fileProbeResult{index: 0, err: errors.New("first probe error")}
	first, anyReady := collectInitialFileProbeResults(results, 2)
	if first != nil {
		t.Fatalf("initial error = %v, want nil for retryable probe errors", first)
	}
	if anyReady {
		t.Fatal("initial results reported a ready file without a ready result")
	}
}

func TestInitialFileProbesStartInParallel(t *testing.T) {
	engine := newEngine(true)
	paths := []string{"first", "second", "third"}
	started := make(chan string, len(paths))
	allowProbes := make(chan struct{})
	var unblockOnce sync.Once
	unblock := func() { unblockOnce.Do(func() { close(allowProbes) }) }
	t.Cleanup(func() {
		unblock()
		engine.Close()
	})

	probe := func(path string) (bool, error) {
		started <- path
		<-allowProbes
		return false, nil
	}
	result := make(chan error, 1)
	go func() {
		_, err := newFileMonitorWithProbe(paths, engine, probe, time.Millisecond)
		result <- err
	}()
	for range paths {
		select {
		case <-started:
		case <-time.After(fileMonitorTestTimeout):
			t.Fatal("initial file probes did not all start before the release barrier")
		}
	}
	engine.stopFiles()
	unblock()
	select {
	case err := <-result:
		if err != nil {
			t.Fatalf("newFileMonitorWithProbe returned error: %v", err)
		}
	case <-time.After(fileMonitorTestTimeout):
		t.Fatal("initial file probes did not complete after the release barrier")
	}
}

func TestInFlightFileProbeErrorIsIgnoredAfterTimedOpen(t *testing.T) {
	engine := newEngineWithGroups(false, []Group{{Members: []Condition{{Kind: "duration", Duration: time.Second}}}})
	defer engine.Close()
	probeStarted := make(chan struct{})
	allowProbe := make(chan struct{})
	monitor := &fileMonitor{
		engine:   engine,
		interval: time.Millisecond,
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
	case <-time.After(fileMonitorTestTimeout):
		t.Fatal("in-flight file probe did not start")
	}
	if err := engine.satisfyDuration(time.Second); err != nil {
		t.Fatalf("satisfyDuration returned error: %v", err)
	}
	select {
	case <-engine.Satisfied():
	case <-time.After(fileMonitorTestTimeout):
		t.Fatal("timed condition did not open engine")
	}
	close(allowProbe)
	select {
	case <-done:
	case <-time.After(fileMonitorTestTimeout):
		t.Fatal("file watcher did not finish after in-flight probe")
	}
	if err := engine.fatalError(); err != nil {
		t.Fatalf("late file fatal = %v, want nil after OPEN", err)
	}
}

func TestTimedAndFileProbeErrorOpenBoundaryHasNoFatal(t *testing.T) {
	engine := newEngineWithGroups(false, []Group{
		{Members: []Condition{{Kind: "duration", Duration: time.Second}}},
		{Members: []Condition{{Kind: "file", Source: "late"}}},
	})
	defer engine.Close()
	probeStarted := make(chan struct{})
	allowProbe := make(chan struct{})
	monitor := &fileMonitor{
		engine:   engine,
		interval: time.Millisecond,
		wait: func(_ string, stop <-chan struct{}, _ time.Duration) bool {
			select {
			case <-stop:
				return false
			default:
				return true
			}
		},
		probe: func(string) (bool, error) {
			close(probeStarted)
			<-allowProbe
			return false, errors.New("transient file probe failure")
		},
	}
	done := make(chan struct{})
	go func() {
		monitor.watchPath("late")
		close(done)
	}()
	select {
	case <-probeStarted:
	case <-time.After(fileMonitorTestTimeout):
		t.Fatal("file probe did not reach the open boundary")
	}
	close(allowProbe)
	if err := engine.satisfyDuration(time.Second); err != nil {
		t.Fatalf("timed event error = %v", err)
	}
	select {
	case <-done:
	case <-time.After(fileMonitorTestTimeout):
		t.Fatal("file watcher did not finish after coordinator opened")
	}
	select {
	case <-engine.Satisfied():
	default:
		t.Fatal("timed condition did not open engine")
	}
	if err := engine.fatalError(); err != nil {
		t.Fatalf("transient file probe error became fatal: %v", err)
	}
}
