package condition

import (
	"errors"
	"sync"
	"testing"
	"time"
)

func TestEngineLatchesANDMembersAndFansOutEquivalentDurations(t *testing.T) {
	engine, err := New([]Group{
		{Members: []Condition{{Kind: "duration", Source: "60s", Duration: time.Minute}, {Kind: "file", Source: "ready"}}},
		{Members: []Condition{{Kind: "duration", Source: "1m", Duration: time.Minute}, {Kind: "file", Source: "other"}}},
	}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer engine.Close()
	if err := engine.Start(); err != nil {
		t.Fatal(err)
	}
	if err := engine.satisfyDuration(time.Minute); err != nil {
		t.Fatal(err)
	}
	select {
	case <-engine.Release():
		t.Fatal("duration alone satisfied an incomplete group")
	default:
	}
	if err := engine.reportFileReady("ready"); err != nil {
		t.Fatal(err)
	}
	select {
	case <-engine.Release():
	case <-time.After(time.Second):
		t.Fatal("AND group did not release")
	}
}

func TestEngineStartsDistinctDurationsFromOneCall(t *testing.T) {
	now := time.Date(2026, time.January, 2, 3, 4, 5, 0, time.UTC)
	var (
		mu     sync.Mutex
		timers []chan time.Time
		delays []time.Duration
	)
	engine, err := New([]Group{
		{Members: []Condition{{Kind: "duration", Source: "1s", Duration: time.Second}}},
		{Members: []Condition{{Kind: "duration", Source: "2s", Duration: 2 * time.Second}}},
	}, Options{
		Now: func() time.Time { return now },
		NewTimer: func(delay time.Duration) (<-chan time.Time, func()) {
			mu.Lock()
			defer mu.Unlock()
			timer := make(chan time.Time, 1)
			timers = append(timers, timer)
			delays = append(delays, delay)
			return timer, func() {}
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer engine.Close()
	if err := engine.Start(); err != nil {
		t.Fatal(err)
	}
	if err := engine.StartDurations(); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	gotDelays := append([]time.Duration(nil), delays...)
	gotTimers := append([]chan time.Time(nil), timers...)
	mu.Unlock()
	if len(gotDelays) != 2 || gotDelays[0] != time.Second || gotDelays[1] != 2*time.Second {
		t.Fatalf("timer delays = %v, want [1s 2s]", gotDelays)
	}
	gotTimers[0] <- now
	select {
	case <-engine.Release():
	case <-time.After(time.Second):
		t.Fatal("first duration did not release its alternative")
	}
}

func TestEngineInitialFileProbeErrorsRemainPendingAndRetry(t *testing.T) {
	var (
		mu    sync.Mutex
		calls int
	)
	engine, err := New([]Group{{Members: []Condition{{Kind: "file", Source: "ready"}}}}, Options{
		FilePollInterval: time.Millisecond,
		FileProbe: func(string) (bool, error) {
			mu.Lock()
			calls++
			call := calls
			mu.Unlock()
			if call == 1 {
				return false, errors.New("transient")
			}
			return true, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer engine.Close()
	if err := engine.Start(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-engine.Release():
	case <-time.After(time.Second):
		t.Fatal("file probe did not retry")
	}
	mu.Lock()
	gotCalls := calls
	mu.Unlock()
	if gotCalls < 2 {
		t.Fatalf("probe calls = %d, want at least 2", gotCalls)
	}
	select {
	case err := <-engine.Failures():
		t.Fatalf("unexpected fatal failure: %v", err)
	default:
	}
}

func TestEngineUsesTypedDurationAndDatetimeValuesForEquivalence(t *testing.T) {
	engine, err := New([]Group{
		{Members: []Condition{{Kind: "duration", Source: "not-a-duration", Duration: time.Minute}, {Kind: "signal", Source: "SIGUSR1"}}},
		{Members: []Condition{{Kind: "duration", Source: "also-not-a-duration", Duration: time.Minute}, {Kind: "signal", Source: "SIGUSR2"}}},
		{Members: []Condition{{Kind: "datetime", Source: "not-RFC3339", Deadline: time.Unix(100, 0).UTC()}, {Kind: "signal", Source: "SIGUSR1"}}},
		{Members: []Condition{{Kind: "datetime", Source: "also-not-RFC3339", Deadline: time.Unix(100, 0).UTC()}, {Kind: "signal", Source: "SIGUSR2"}}},
	}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer engine.Close()
	duration, durationOK := durationReleaseValue(engine.groups[0].members[0].condition)
	if duration != time.Minute || !durationOK {
		t.Fatal("duration value was not taken from the typed field")
	}
	deadline, deadlineOK := datetimeReleaseValue(engine.groups[2].members[0].condition)
	if !deadline.Equal(time.Unix(100, 0).UTC()) || !deadlineOK {
		t.Fatal("datetime value was not taken from the typed field")
	}
	if conditionKeyFor(engine.groups[0].members[0].condition) != conditionKeyFor(engine.groups[1].members[0].condition) {
		t.Fatal("equivalent typed durations did not share a key")
	}
	if conditionKeyFor(engine.groups[2].members[0].condition) != conditionKeyFor(engine.groups[3].members[0].condition) {
		t.Fatal("equivalent typed datetimes did not share a key")
	}
}

func TestEngineRootSelectionStopsTimeAndFileMonitorsButCloseStopsSignals(t *testing.T) {
	var (
		mu      sync.Mutex
		stopped int
		timer   = make(chan time.Time)
	)
	engine, err := New([]Group{{Members: []Condition{{Kind: "duration", Duration: time.Second}, {Kind: "file", Source: "pending"}}}}, Options{
		NewTimer: func(time.Duration) (<-chan time.Time, func()) {
			return timer, func() {
				mu.Lock()
				stopped++
				mu.Unlock()
			}
		},
		FileProbe:        func(string) (bool, error) { return false, nil },
		FilePollInterval: time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := engine.Start(); err != nil {
		t.Fatal(err)
	}
	if err := engine.StartDurations(); err != nil {
		t.Fatal(err)
	}
	if err := engine.satisfyDuration(time.Second); err != nil {
		t.Fatal(err)
	}
	if err := engine.satisfyCondition("file", "pending"); err != nil {
		t.Fatal(err)
	}
	select {
	case <-engine.Satisfied():
	case <-time.After(time.Second):
		t.Fatal("root condition did not become satisfied")
	}
	mu.Lock()
	gotStopped := stopped
	mu.Unlock()
	if gotStopped == 0 {
		t.Fatal("root selection did not stop the outstanding timer")
	}
	engine.Close()
}

func TestEngineInitializesTimedMonitorBeforeSignalMonitor(t *testing.T) {
	engine, err := New([]Group{{Members: []Condition{{Kind: "signal", Source: "SIGUSR1"}}}}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer engine.Close()

	// Let the injected monitor callback represent a signal consumed while the
	// signal monitor is being constructed. This makes the startup ordering
	// observable without relying on scheduler timing or an OS signal.
	engine.mu.Lock()
	engine.initializing = false
	engine.mu.Unlock()
	engine.signalMonitorFactory = func(_ []string, current *Engine) (*releaseMonitor, error) {
		if current.timed == nil {
			t.Error("timed monitor was not initialized before signal monitor construction")
		}
		if err := current.satisfyCondition("signal", "SIGUSR1"); err != nil {
			t.Errorf("satisfy signal: %v", err)
		}
		return &releaseMonitor{engine: current}, nil
	}

	if err := engine.Start(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-engine.Satisfied():
	default:
		t.Fatal("signal did not select the root condition")
	}

	engine.timed.mu.Lock()
	timedClosed := engine.timed.closed
	engine.timed.mu.Unlock()
	if !timedClosed {
		t.Fatal("timed monitor was installed after signal selection")
	}
}

func TestEngineCompleteEmptyReportsPriorSelection(t *testing.T) {
	engine, err := New([]Group{{Members: []Condition{{Kind: "duration", Duration: 0}}}}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if err := engine.Start(); err != nil {
		t.Fatal(err)
	}
	selected, err := engine.CompleteEmpty()
	if err != nil {
		t.Fatal(err)
	}
	if !selected {
		t.Fatal("empty completion suppressed an already-selected root")
	}
	engine.Close()
}

func TestEngineCompleteEmptyPreventsLaterSelection(t *testing.T) {
	engine, err := New([]Group{{Members: []Condition{{Kind: "duration", Duration: time.Hour}}}}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	selected, err := engine.CompleteEmpty()
	if err != nil {
		t.Fatal(err)
	}
	if selected {
		t.Fatal("unsatisfied root reported selection")
	}
	if err := engine.satisfyDuration(time.Hour); err != nil {
		t.Fatal(err)
	}
	select {
	case <-engine.Satisfied():
		t.Fatal("monitor result selected root after empty completion")
	default:
	}
}
