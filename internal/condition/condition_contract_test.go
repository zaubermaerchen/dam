package condition

// These tests pin the condition package's typed composition and monitor
// lifecycle. Runtime event emission is intentionally tested in cmd/dam.

import (
	"errors"
	"slices"
	"sync"
	"testing"
	"time"
)

func TestEngineFansOutEquivalentDurationAndLatchesANDMembers(t *testing.T) {
	engine, err := New([]Group{
		{Members: []Condition{
			{Kind: "duration", Source: "not-authoritative", Duration: time.Minute},
			{Kind: "file", Source: "first"},
		}},
		{Members: []Condition{
			{Kind: "duration", Source: "also-not-authoritative", Duration: time.Minute},
			{Kind: "file", Source: "second"},
		}},
	}, Options{FileProbe: func(string) (bool, error) { return false, nil }})
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
	if got := remainingMembers(engine); !slices.Equal(got, []int{1, 1}) {
		t.Fatalf("equivalent duration did not fan out to both groups: remaining = %v", got)
	}

	if err := engine.reportFileReady("first"); err != nil {
		t.Fatal(err)
	}
	select {
	case <-engine.Satisfied():
	case <-time.After(time.Second):
		t.Fatal("completed AND group did not select the root condition")
	}
	// The second alternative remains latched but is not needed after the first
	// alternative wins. Repeating the same physical events is harmless.
	if err := engine.satisfyDuration(time.Minute); err != nil {
		t.Fatal(err)
	}
	if err := engine.reportFileReady("first"); err != nil {
		t.Fatal(err)
	}
}

func TestEngineUsesTypedDurationAndDeadlineForTimersAndEquivalence(t *testing.T) {
	base := time.Date(2026, time.January, 2, 3, 4, 5, 0, time.UTC)
	var (
		mu       sync.Mutex
		delays   []time.Duration
		timers   []chan time.Time
		stopCall []int
	)
	engine, err := New([]Group{
		{Members: []Condition{{Kind: "duration", Source: "invalid-duration", Duration: 2 * time.Second}}},
		{Members: []Condition{{Kind: "duration", Source: "another-invalid-duration", Duration: 2 * time.Second}}},
		{Members: []Condition{{Kind: "datetime", Source: "invalid-datetime", Deadline: base.Add(3 * time.Second)}}},
	}, Options{
		Now: func() time.Time { return base },
		NewTimer: func(delay time.Duration) (<-chan time.Time, func()) {
			mu.Lock()
			defer mu.Unlock()
			timer := make(chan time.Time, 1)
			delays = append(delays, delay)
			timers = append(timers, timer)
			index := len(stopCall)
			stopCall = append(stopCall, 0)
			return timer, func() {
				mu.Lock()
				stopCall[index]++
				mu.Unlock()
			}
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
	rawDelays := slices.Clone(delays)
	gotTimers := slices.Clone(timers)
	mu.Unlock()
	gotDelays := slices.Clone(rawDelays)
	slices.Sort(gotDelays)
	if want := []time.Duration{2 * time.Second, 3 * time.Second}; !slices.Equal(gotDelays, want) {
		t.Fatalf("timer delays = %v, want %v", gotDelays, want)
	}
	// Equal typed values use one physical timer even when their source strings
	// are unrelated. The malformed Source values above must never be reparsed.
	if len(gotDelays) != 2 {
		t.Fatalf("timer count = %d, want one datetime and one shared duration timer", len(gotDelays))
	}

	var durationTimer chan time.Time
	for index, delay := range rawDelays {
		if delay == 2*time.Second {
			durationTimer = gotTimers[index]
			break
		}
	}
	if durationTimer == nil {
		t.Fatal("shared duration timer was not created")
	}
	durationTimer <- base
	select {
	case <-engine.Satisfied():
	case <-time.After(time.Second):
		t.Fatal("typed duration timer did not satisfy its group")
	}
}

func TestEngineStartsDurationsFromOneSharedEpochAndCancelsLosers(t *testing.T) {
	base := time.Date(2026, time.January, 2, 3, 4, 5, 0, time.UTC)
	var (
		mu       sync.Mutex
		nowCalls int
		delays   []time.Duration
		timers   []chan time.Time
		stopped  []int
	)
	engine, err := New([]Group{
		{Members: []Condition{{Kind: "duration", Duration: time.Second}}},
		{Members: []Condition{{Kind: "duration", Duration: 2 * time.Second}}},
	}, Options{
		Now: func() time.Time {
			mu.Lock()
			defer mu.Unlock()
			value := base.Add(time.Duration(nowCalls) * time.Hour)
			nowCalls++
			return value
		},
		NewTimer: func(delay time.Duration) (<-chan time.Time, func()) {
			mu.Lock()
			defer mu.Unlock()
			timer := make(chan time.Time, 1)
			delays = append(delays, delay)
			timers = append(timers, timer)
			index := len(stopped)
			stopped = append(stopped, 0)
			return timer, func() {
				mu.Lock()
				stopped[index]++
				mu.Unlock()
			}
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer engine.Close()
	if err := engine.Start(); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	nowCalls = 0
	mu.Unlock()
	if err := engine.StartDurations(); err != nil {
		t.Fatal(err)
	}

	mu.Lock()
	gotCalls := nowCalls
	gotDelays := slices.Clone(delays)
	gotTimers := slices.Clone(timers)
	mu.Unlock()
	if gotCalls != 1 {
		t.Fatalf("duration start called Now %d times, want one shared epoch", gotCalls)
	}
	if want := []time.Duration{time.Second, 2 * time.Second}; !slices.Equal(gotDelays, want) {
		t.Fatalf("duration delays = %v, want %v", gotDelays, want)
	}

	gotTimers[0] <- base
	select {
	case <-engine.Satisfied():
	case <-time.After(time.Second):
		t.Fatal("first duration did not select the root condition")
	}
	deadline := time.Now().Add(time.Second)
	for {
		mu.Lock()
		loserStopped := len(stopped) > 1 && stopped[1] > 0
		mu.Unlock()
		if loserStopped {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("unselected duration timer was not cancelled after root selection")
		}
		time.Sleep(time.Millisecond)
	}
}

func TestEngineArmsDatetimesBeforeInitialFileProbeBarrier(t *testing.T) {
	base := time.Date(2026, time.January, 2, 3, 4, 5, 0, time.UTC)
	var engine *Engine
	var (
		mu            sync.Mutex
		timerStarted  bool
		probeSawTimer bool
	)
	engine, err := New([]Group{
		{Members: []Condition{{Kind: "datetime", Deadline: base.Add(time.Hour)}}},
		{Members: []Condition{{Kind: "file", Source: "pending"}}},
	}, Options{
		Now: func() time.Time { return base },
		NewTimer: func(time.Duration) (<-chan time.Time, func()) {
			mu.Lock()
			timerStarted = true
			mu.Unlock()
			return make(chan time.Time), func() {}
		},
		FileProbe: func(string) (bool, error) {
			mu.Lock()
			probeSawTimer = timerStarted
			mu.Unlock()
			return false, nil
		},
		FilePollInterval: time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer engine.Close()
	if err := engine.Start(); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	got := probeSawTimer
	mu.Unlock()
	if !got {
		t.Fatal("initial file probe ran before datetime monitor was armed")
	}
}

func TestEngineRetriesNonRegularAndErrorFileProbes(t *testing.T) {
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
			switch call {
			case 1:
				return false, errors.New("transient stat error")
			case 2:
				return false, nil // non-regular result remains pending
			default:
				return true, nil
			}
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
	case <-engine.Satisfied():
	case <-time.After(time.Second):
		t.Fatal("file monitor did not retry pending probes")
	}
	mu.Lock()
	gotCalls := calls
	mu.Unlock()
	if gotCalls < 3 {
		t.Fatalf("probe calls = %d, want error, pending, and regular attempts", gotCalls)
	}
	select {
	case err := <-engine.Failures():
		t.Fatalf("retryable file probe reported fatal failure: %v", err)
	default:
	}
}

func TestEngineDoesNotDeduplicateRepeatedFilePaths(t *testing.T) {
	var (
		mu    sync.Mutex
		calls int
	)
	engine, err := New([]Group{{Members: []Condition{
		{Kind: "file", Source: "same"},
		{Kind: "file", Source: "same"},
	}}}, Options{
		FileProbe: func(string) (bool, error) {
			mu.Lock()
			calls++
			mu.Unlock()
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
	case <-engine.Satisfied():
	case <-time.After(time.Second):
		t.Fatal("repeated file path did not satisfy its AND group")
	}
	mu.Lock()
	gotCalls := calls
	mu.Unlock()
	if gotCalls != 2 {
		t.Fatalf("file probe calls = %d, want one probe per repeated argument", gotCalls)
	}
}

func TestEngineIgnoresInFlightFileResultAfterClose(t *testing.T) {
	probeStarted := make(chan struct{})
	allowProbe := make(chan struct{})
	var (
		mu    sync.Mutex
		calls int
	)
	engine, err := New([]Group{{Members: []Condition{{Kind: "file", Source: "pending"}}}}, Options{
		FilePollInterval: time.Millisecond,
		FileProbe: func(string) (bool, error) {
			mu.Lock()
			calls++
			call := calls
			mu.Unlock()
			if call == 1 {
				return false, nil
			}
			close(probeStarted)
			<-allowProbe
			return true, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := engine.Start(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-probeStarted:
	case <-time.After(time.Second):
		t.Fatal("file monitor did not start a polling probe")
	}
	engine.Close()
	close(allowProbe)
	time.Sleep(10 * time.Millisecond)
	select {
	case <-engine.Satisfied():
		t.Fatal("in-flight file result opened a closed engine")
	default:
	}
}

func remainingMembers(engine *Engine) []int {
	engine.mu.Lock()
	defer engine.mu.Unlock()
	remaining := make([]int, len(engine.groups))
	for index, group := range engine.groups {
		remaining[index] = group.remaining
	}
	return remaining
}
