package main

// This file verifies dam's argument validation and delayed stdin forwarding.

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

const (
	readBufferSize = 32 * 1024
	testTimeout    = 5 * time.Second
)

const expectedHelpText = `Usage:
  dam CONDITION [--or CONDITION]... [--buffer-size SIZE]
  dam --help
  dam --version
  dam --describe

Hold pipeline output until a release condition is met.

Arguments:
  CONDITION
        A condition is one of:
          duration:DURATION
              A positive Go duration (such as 500ms, 3s, or 2m) starts after
              the first non-empty stdin read completes. A 0s duration
              satisfies its condition immediately.
              Multiple positive duration conditions share that starting read.
          datetime:YYYY-MM-DDTHH:MM[:SS]
              An absolute local datetime monitored from startup. Multiple
              datetime conditions are allowed.
          datetime:YYYY-MM-DDTHH:MM:SS[Z|+HH:MM|-HH:MM]
              RFC3339 form; explicit timezones require seconds. Fractional
              seconds and named timezones are invalid.
          signal:USR1, signal:SIGUSR1, signal:USR2, signal:SIGUSR2
              Release on the configured Unix signal. Alias spellings are
              equivalent on supported Unix targets.
          file:PATH
              Release when PATH resolves to a regular file on any target.
        Conditions joined by " && " inside one argument form an AND group.
        Quote AND groups so the shell passes " && " literally. Every member
        is latched once satisfied. Use --or between alternative conditions.

Options:
  --or CONDITION
        Make CONDITION an alternative to the preceding condition. May be
        written as --or=CONDITION.

  --buffer-size SIZE
        Set the maximum pre-release buffer size (default: 64K).
        SIZE is a positive byte count or a binary K/k, M/m, or G/g value.
        Also accepted as --buffer-size=SIZE.

  --events-fd N
        Emit release-selected and stream-open JSONL events to file descriptor N.
        Also accepted as --events-fd=N. Event transport failures disable events
        with one warning while the primary stream continues.

Notes:
        Equivalent duration values and resolved datetime values share one
        latched event. Time and file monitors stop after release or empty
        stdin reaches EOF.

  -h, --help
        Show this help and exit.

  --version
        Show version and exit.

  --describe
        Show a compact machine-readable JSON description and exit.
`

func TestDocumentationDescribesV040MigrationAndCurrentGrammar(t *testing.T) {
	readme := readRepositoryDocumentation(t, "README.md")
	agents := readRepositoryDocumentation(t, "AGENTS.md")

	migrationHeading := "## Migrating from v0.3.x"
	migrationStart := strings.Index(readme, migrationHeading)
	if migrationStart < 0 {
		t.Fatalf("README.md is missing %q", migrationHeading)
	}

	for _, want := range []string{
		"dam --describe",
		"schema_version",
		"condition_forms",
		"stream_semantics",
		"state_machine",
		"side_effects",
		"first-non-empty-read-completion",
		"last value wins",
		"v0.4.0 is a breaking release",
		"dam 30s\n  -> dam duration:30s",
		"dam 2026-09-03T18:00\n  -> dam datetime:2026-09-03T18:00",
		"dam --release-on signal:USR1\n  -> dam signal:USR1",
		"dam --release-on=signal:USR1\n  -> dam signal:USR1",
		"dam --release-on signal:USR1 --release-on file:/tmp/ready\n  -> dam signal:USR1 --or file:/tmp/ready",
		"--or CONDITION",
		"--or=CONDITION",
		" && ",
		"latched",
		"multiple distinct durations",
		"Multiple distinct\ndatetime conditions",
		"datetime:YYYY-MM-DDTHH:MM:SS[Z|+HH:MM|-HH:MM]",
		"strict RFC 3339",
		"explicit UTC timezone",
		"Equivalent duration values",
		"equivalent datetime values",
		"monitoring stop",
		"--events-fd",
		"release-selected",
		"stream-open",
	} {
		if !strings.Contains(readme, want) {
			t.Errorf("README.md is missing documentation %q", want)
		}
	}

	// Historical spellings belong only in the migration section; keeping them
	// out of every other section prevents the README from advertising removed
	// syntax as supported.
	migrationEnd := len(readme)
	sectionRemainder := readme[migrationStart+len(migrationHeading):]
	if nextHeading := strings.Index(sectionRemainder, "\n## "); nextHeading >= 0 {
		migrationEnd = migrationStart + len(migrationHeading) + nextHeading + 1
	}
	activeREADME := readme[:migrationStart] + readme[migrationEnd:]
	for _, obsolete := range []string{"--release-on", "dam 30s", "dam 2026-09-03T18:00"} {
		if strings.Contains(activeREADME, obsolete) {
			t.Errorf("README.md advertises obsolete syntax outside migration notes: %q", obsolete)
		}
	}

	for _, want := range []string{
		"dam CONDITION [--or CONDITION]... [--buffer-size SIZE]",
		"dam --describe",
		"duration:DURATION",
		"datetime:",
		"signal:",
		"file:",
		"&&",
		"latch",
		"最初の非空 read",
		"起動時",
		"停止",
		"signal を含まない duration / datetime / file の構成（組合せ含む）",
		"--describe",
		"stdin を読まず",
		"--events-fd",
		"release-selected",
		"stream-open",
	} {
		if !strings.Contains(agents, want) {
			t.Errorf("AGENTS.md is missing current-contract documentation %q", want)
		}
	}
	if strings.Contains(agents, "--release-on") {
		t.Error("AGENTS.md advertises removed --release-on syntax")
	}
}

func readRepositoryDocumentation(t *testing.T, name string) string {
	t.Helper()
	content, err := os.ReadFile(filepath.Join("..", "..", name))
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	return strings.ReplaceAll(string(content), "\r\n", "\n")
}

func TestRunPreservesBinaryInputWithZeroDelay(t *testing.T) {
	input := make([]byte, 256*3)
	for i := range input {
		input[i] = byte(i)
	}

	var output bytes.Buffer
	var diagnostics bytes.Buffer
	if status := run([]string{"duration:0s"}, bytes.NewReader(input), &output, &diagnostics); status != 0 {
		t.Fatalf("run status = %d, diagnostics = %q", status, diagnostics.String())
	}
	if !bytes.Equal(output.Bytes(), input) {
		t.Fatalf("output changed input bytes")
	}
	if diagnostics.Len() != 0 {
		t.Fatalf("successful run wrote diagnostics: %q", diagnostics.String())
	}
}

func TestRunPrintsVersionWithoutReadingInput(t *testing.T) {
	input := &trackingReader{}
	var output, diagnostics bytes.Buffer

	if status := run([]string{"--version"}, input, &output, &diagnostics); status != 0 {
		t.Fatalf("run status = %d, diagnostics = %q", status, diagnostics.String())
	}
	if got, want := output.String(), "dam dev\n"; got != want {
		t.Fatalf("version output = %q, want %q", got, want)
	}
	if diagnostics.Len() != 0 {
		t.Fatalf("successful version run wrote diagnostics: %q", diagnostics.String())
	}
	if input.reads != 0 {
		t.Fatalf("version run read stdin %d times", input.reads)
	}
}

func TestRunPrintsConfiguredVersion(t *testing.T) {
	originalVersion := version
	version = "v1.2.3"
	t.Cleanup(func() { version = originalVersion })

	var output, diagnostics bytes.Buffer
	if status := run([]string{"--version"}, strings.NewReader("ignored"), &output, &diagnostics); status != 0 {
		t.Fatalf("run status = %d, diagnostics = %q", status, diagnostics.String())
	}
	if got, want := output.String(), "dam v1.2.3\n"; got != want {
		t.Fatalf("version output = %q, want %q", got, want)
	}
}

func TestRunReportsVersionOutputErrors(t *testing.T) {
	outputErr := errors.New("version output failed")
	var diagnostics bytes.Buffer
	if status := run([]string{"--version"}, &trackingReader{}, errorWriter{err: outputErr}, &diagnostics); status == 0 {
		t.Fatal("version output error unexpectedly succeeded")
	}
	if !strings.Contains(diagnostics.String(), outputErr.Error()) {
		t.Fatalf("version diagnostic = %q, want %q", diagnostics.String(), outputErr)
	}
}

func TestRunPrintsHelpWithoutReadingInputOrStartingReadiness(t *testing.T) {
	for _, arg := range []string{"-h", "--help"} {
		t.Run(arg, func(t *testing.T) {
			input := &trackingReader{}
			var output, diagnostics bytes.Buffer
			ready := false
			status, cleanup := executeWithReady([]string{arg}, input, &output, &diagnostics, func() {
				ready = true
			})
			if cleanup != nil {
				t.Fatal("help created a release monitor")
			}
			if status != 0 {
				t.Fatalf("help status = %d, diagnostics = %q", status, diagnostics.String())
			}
			if got, want := output.String(), expectedHelpText; got != want {
				t.Fatalf("help output = %q, want %q", got, want)
			}
			if diagnostics.Len() != 0 {
				t.Fatalf("successful help wrote diagnostics: %q", diagnostics.String())
			}
			if input.reads != 0 {
				t.Fatalf("help read stdin %d times", input.reads)
			}
			if ready {
				t.Fatal("help started release readiness")
			}
		})
	}
}

func TestRunHelpTakesPriorityOverOtherArguments(t *testing.T) {
	tests := [][]string{
		{"--version", "--help"},
		{"--help", "--version"},
		{"--unknown", "-h"},
		{"--release-on", "--help"},
		{"3s", "--help"},
		{"-h", "--help"},
	}

	for index, args := range tests {
		var output, diagnostics bytes.Buffer
		input := &trackingReader{}
		status := run(args, input, &output, &diagnostics)
		if status != 0 {
			t.Fatalf("case %d help status = %d, diagnostics = %q", index, status, diagnostics.String())
		}
		if diagnostics.Len() != 0 {
			t.Fatalf("case %d successful help wrote diagnostics: %q", index, diagnostics.String())
		}
		if input.reads != 0 {
			t.Fatalf("case %d help read stdin %d times", index, input.reads)
		}
		if got, want := output.String(), expectedHelpText; got != want {
			t.Fatalf("case %d help output = %q, want %q", index, got, want)
		}
	}
}

func TestRunReportsHelpOutputErrorsWithoutReadingInput(t *testing.T) {
	outputErr := errors.New("help output failed")
	input := &trackingReader{}
	var diagnostics bytes.Buffer
	ready := false
	status, cleanup := executeWithReady([]string{"--help"}, input, errorWriter{err: outputErr}, &diagnostics, func() {
		ready = true
	})
	if cleanup != nil {
		t.Fatal("help created a release monitor after output error")
	}
	if status == 0 {
		t.Fatal("help output error unexpectedly succeeded")
	}
	if !strings.Contains(diagnostics.String(), outputErr.Error()) {
		t.Fatalf("help diagnostic = %q, want %q", diagnostics.String(), outputErr)
	}
	if input.reads != 0 {
		t.Fatalf("help read stdin %d times", input.reads)
	}
	if ready {
		t.Fatal("help started release readiness after output error")
	}
}

func TestRunTreatsNearHelpArgumentsAsErrors(t *testing.T) {
	for _, args := range [][]string{
		{"--help=x"},
		{"--release-on=--help"},
		{"-help"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			var output, diagnostics bytes.Buffer
			if status := run(args, strings.NewReader("input"), &output, &diagnostics); status == 0 {
				t.Fatal("near-help argument unexpectedly succeeded")
			}
			if output.Len() != 0 {
				t.Fatalf("near-help argument wrote stdout: %q", output.String())
			}
			if diagnostics.Len() == 0 {
				t.Fatal("near-help argument produced no stderr diagnostic")
			}
		})
	}
}

func TestRunPreservesDelayedBinaryInput(t *testing.T) {
	const delay = 100 * time.Millisecond
	input := eofReader{data: []byte{0x00, 0xff, 0x01, 0xfe, 0x7f}}
	output := &lockedBuffer{writeTimes: make(chan time.Time, 1)}
	var diagnostics bytes.Buffer
	startedAt := time.Now()
	status := make(chan int, 1)
	go func() {
		status <- run([]string{"duration:" + delay.String()}, input, output, &diagnostics)
	}()

	var wroteAt time.Time
	select {
	case wroteAt = <-output.writeTimes:
	case <-time.After(testTimeout):
		t.Fatal("run did not release binary input after the delay")
	}
	if elapsed := wroteAt.Sub(startedAt); elapsed < delay {
		t.Fatalf("binary input released after %s, want at least %s", elapsed, delay)
	}
	if got, want := output.Bytes(), input.data; !bytes.Equal(got, want) {
		t.Fatalf("output = %x, want %x", got, want)
	}

	select {
	case gotStatus := <-status:
		if gotStatus != 0 {
			t.Fatalf("run status = %d, diagnostics = %q", gotStatus, diagnostics.String())
		}
	case <-time.After(time.Second):
		t.Fatal("run did not complete after releasing binary input")
	}
}

func TestRunStartsDelayOnFirstByteRead(t *testing.T) {
	const delay = 100 * time.Millisecond
	input := &firstReadGate{
		data:    []byte("delayed"),
		started: make(chan struct{}),
		release: make(chan struct{}),
	}
	output := &lockedBuffer{writeTimes: make(chan time.Time, 1)}
	var diagnostics bytes.Buffer
	status := make(chan int, 1)
	go func() {
		status <- run([]string{"duration:" + delay.String()}, input, output, &diagnostics)
	}()

	select {
	case <-input.started:
	case <-time.After(time.Second):
		t.Fatal("run did not attempt the first read")
	}

	// Keep the first read blocked until a process-start timer would have
	// expired. The actual delay must begin only when the read returns bytes.
	time.Sleep(3 * delay)
	releasedAt := time.Now()
	close(input.release)
	var wroteAt time.Time
	select {
	case wroteAt = <-output.writeTimes:
	case <-time.After(testTimeout):
		t.Fatal("run did not release output after the first-byte delay")
	}
	if elapsed := wroteAt.Sub(releasedAt); elapsed < delay {
		t.Fatalf("output released after %s, want at least %s", elapsed, delay)
	}

	select {
	case gotStatus := <-status:
		if gotStatus != 0 {
			t.Fatalf("run status = %d, diagnostics = %q", gotStatus, diagnostics.String())
		}
	case <-time.After(testTimeout):
		t.Fatal("run did not complete after the delay")
	}
	if got, want := output.String(), "delayed"; got != want {
		t.Fatalf("output = %q, want %q", got, want)
	}
}

func TestRunDoesNotReleaseOnEOFBeforeDelay(t *testing.T) {
	const delay = 100 * time.Millisecond
	output := &lockedBuffer{writeTimes: make(chan time.Time, 1)}
	var diagnostics bytes.Buffer
	startedAt := time.Now()
	status := make(chan int, 1)
	go func() {
		status <- run([]string{"duration:" + delay.String()}, eofReader{data: []byte("before eof")}, output, &diagnostics)
	}()

	var wroteAt time.Time
	select {
	case wroteAt = <-output.writeTimes:
	case <-time.After(testTimeout):
		t.Fatal("run did not release EOF input after the delay")
	}
	if elapsed := wroteAt.Sub(startedAt); elapsed < delay {
		t.Fatalf("EOF input released after %s, want at least %s", elapsed, delay)
	}

	select {
	case gotStatus := <-status:
		if gotStatus != 0 {
			t.Fatalf("run status = %d, diagnostics = %q", gotStatus, diagnostics.String())
		}
	case <-time.After(testTimeout):
		t.Fatal("run did not complete after EOF delay")
	}
	if got, want := output.String(), "before eof"; got != want {
		t.Fatalf("output = %q, want %q", got, want)
	}
}

func TestRunDoesNotStartDelayForEmptyInput(t *testing.T) {
	status := make(chan int, 1)
	var output, diagnostics bytes.Buffer
	go func() {
		status <- run([]string{"duration:1h"}, strings.NewReader(""), &output, &diagnostics)
	}()

	select {
	case gotStatus := <-status:
		if gotStatus != 0 {
			t.Fatalf("run status = %d, diagnostics = %q", gotStatus, diagnostics.String())
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("empty input was delayed")
	}
	if output.Len() != 0 {
		t.Fatalf("empty input produced output: %q", output.String())
	}
}

func TestRunAppliesBackpressureBeforeRelease(t *testing.T) {
	const delay = 100 * time.Millisecond
	input := &backpressureReader{
		chunks:         [][]byte{[]byte("a"), []byte("b"), []byte("c")},
		firstRead:      make(chan time.Time, 1),
		secondStarted:  make(chan time.Time, 1),
		blockedRead:    make(chan time.Time, 1),
		releaseBlocked: make(chan struct{}),
	}
	output := &lockedBuffer{writeTimes: make(chan time.Time, 3)}
	var diagnostics bytes.Buffer
	status := make(chan int, 1)
	go func() {
		status <- run([]string{"duration:" + delay.String()}, input, output, &diagnostics)
	}()

	var firstReadAt time.Time
	select {
	case firstReadAt = <-input.firstRead:
	case <-time.After(time.Second):
		t.Fatal("run did not attempt the first read")
	}
	select {
	case <-input.secondStarted:
	case <-time.After(testTimeout):
		t.Fatal("run did not continue reading while the delay was pending")
	}
	select {
	case <-input.blockedRead:
	case <-time.After(testTimeout):
		t.Fatal("run did not apply backpressure to a blocked subsequent read")
	}
	select {
	case <-output.writeTimes:
		t.Fatal("output released before the requested delay")
	default:
	}

	select {
	case wroteAt := <-output.writeTimes:
		if elapsed := wroteAt.Sub(firstReadAt); elapsed < delay {
			t.Fatalf("output released after %s, want at least %s", elapsed, delay)
		}
	case <-time.After(testTimeout):
		t.Fatal("timer did not release output while a later read was blocked")
	}
	close(input.releaseBlocked)

	select {
	case gotStatus := <-status:
		if gotStatus != 0 {
			t.Fatalf("run status = %d, diagnostics = %q", gotStatus, diagnostics.String())
		}
	case <-time.After(time.Second):
		t.Fatal("run did not complete after releasing the blocked read")
	}
	if got, want := output.String(), "abc"; got != want {
		t.Fatalf("output = %q, want %q", got, want)
	}
}

func TestRunBoundsPreReleaseReadAhead(t *testing.T) {
	const delay = 200 * time.Millisecond
	input := &boundedReadReader{
		calls:   make(chan int, 4),
		release: make(chan struct{}),
	}
	output := &lockedBuffer{writeTimes: make(chan time.Time, 4)}
	var diagnostics bytes.Buffer
	status := make(chan int, 1)
	go func() {
		status <- run([]string{"duration:" + delay.String()}, input, output, &diagnostics)
	}()

	bytesRead := 0
	for i := 0; i < 3; i++ {
		select {
		case n := <-input.calls:
			bytesRead += n
		case <-time.After(time.Second):
			t.Fatal("run did not fill its pre-release read-ahead")
		}
	}
	if bytesRead > preReleaseBufferSize {
		t.Fatalf("read %d bytes before release, want at most %d", bytesRead, preReleaseBufferSize)
	}
	select {
	case <-output.writeTimes:
		t.Fatal("output released before the requested delay")
	default:
	}

	select {
	case <-output.writeTimes:
	case <-time.After(testTimeout):
		t.Fatal("run did not release bounded pre-release data")
	}
	close(input.release)

	select {
	case gotStatus := <-status:
		if gotStatus != 0 {
			t.Fatalf("run status = %d, diagnostics = %q", gotStatus, diagnostics.String())
		}
	case <-time.After(time.Second):
		t.Fatal("run did not complete after releasing bounded pre-release data")
	}
	if got, want := output.Bytes(), input.data; !bytes.Equal(got, want) {
		t.Fatalf("output changed input: got %d bytes, want %d", len(got), len(want))
	}
}

func TestRunFillsBoundedPreReleaseBufferWithFragmentedReads(t *testing.T) {
	const delay = 2 * time.Second
	const fragmentedReadSize = 1024
	const readCount = preReleaseBufferSize / fragmentedReadSize
	input := &fragmentedReadReader{
		readStarted: make(chan int, readCount+1),
		release:     make(chan struct{}),
	}
	t.Cleanup(input.unblock)
	output := &lockedBuffer{writeTimes: make(chan time.Time, 1)}
	var diagnostics bytes.Buffer
	status := make(chan int, 1)
	go func() {
		status <- run([]string{"duration:" + delay.String()}, input, output, &diagnostics)
	}()

	for wantRead := 1; wantRead <= readCount; wantRead++ {
		select {
		case gotRead := <-input.readStarted:
			if gotRead != wantRead {
				t.Fatalf("read sequence = %d, want %d", gotRead, wantRead)
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("run started only %d of %d pre-release reads", wantRead-1, readCount)
		}
	}
	select {
	case gotRead := <-input.readStarted:
		t.Fatalf("started read %d before release, want no read beyond capacity", gotRead)
	default:
	}
	select {
	case <-output.writeTimes:
		t.Fatal("output released before the requested delay")
	default:
	}

	select {
	case <-output.writeTimes:
	case <-time.After(testTimeout):
		t.Fatal("run did not release the bounded pre-release buffer")
	}
	input.unblock()

	select {
	case gotStatus := <-status:
		if gotStatus != 0 {
			t.Fatalf("run status = %d, diagnostics = %q", gotStatus, diagnostics.String())
		}
	case <-time.After(time.Second):
		t.Fatal("run did not complete after releasing fragmented input")
	}
	if got, want := output.Bytes(), input.data; !bytes.Equal(got, want) {
		t.Fatalf("output changed fragmented input: got %d bytes, want %d", len(got), len(want))
	}
}

func TestRunPassesThroughAfterReleaseWithoutSecondGate(t *testing.T) {
	const delay = 100 * time.Millisecond
	input := &postReleaseReader{release: make(chan struct{})}
	output := &lockedBuffer{writeTimes: make(chan time.Time, 2)}
	var diagnostics bytes.Buffer
	status := make(chan int, 1)
	go func() {
		status <- run([]string{"duration:" + delay.String()}, input, output, &diagnostics)
	}()

	select {
	case <-output.writeTimes:
	case <-time.After(testTimeout):
		t.Fatal("run did not release the first input")
	}
	secondReleaseAt := time.Now()
	close(input.release)

	select {
	case wroteAt := <-output.writeTimes:
		if elapsed := wroteAt.Sub(secondReleaseAt); elapsed >= delay {
			t.Fatalf("post-release input was gated for %s, want less than %s", elapsed, delay)
		}
	case <-time.After(testTimeout):
		t.Fatal("run did not pass through post-release input")
	}

	select {
	case gotStatus := <-status:
		if gotStatus != 0 {
			t.Fatalf("run status = %d, diagnostics = %q", gotStatus, diagnostics.String())
		}
	case <-time.After(time.Second):
		t.Fatal("run did not complete after post-release input")
	}
	if got, want := output.String(), "firstsecond"; got != want {
		t.Fatalf("output = %q, want %q", got, want)
	}
}

func TestRunAbsoluteDeadlineReleasesBeforeFirstInput(t *testing.T) {
	now := time.Date(2026, time.January, 2, 3, 4, 0, 0, time.UTC)
	timerFired := make(chan time.Time, 1)
	clock := runtimeClock{
		now:      func() time.Time { return now },
		location: time.UTC,
		newTimer: func(time.Duration) (<-chan time.Time, func()) {
			return timerFired, func() {}
		},
	}
	input := &firstReadGate{
		data:    []byte("deadline-opened"),
		started: make(chan struct{}),
		release: make(chan struct{}),
	}
	output := &lockedBuffer{writeTimes: make(chan time.Time, 1)}
	var diagnostics bytes.Buffer
	status := make(chan int, 1)
	go func() {
		status <- runWithClock([]string{"datetime:2026-01-02T03:04:05"}, input, output, &diagnostics, clock)
	}()

	select {
	case <-input.started:
	case <-time.After(testTimeout):
		t.Fatal("run did not attempt the first read")
	}
	timerFired <- now
	select {
	case <-output.writeTimes:
		t.Fatal("deadline wrote output before the blocked first read completed")
	default:
	}
	close(input.release)
	select {
	case got := <-status:
		if got != 0 {
			t.Fatalf("run status = %d, diagnostics = %q", got, diagnostics.String())
		}
	case <-time.After(testTimeout):
		t.Fatal("run did not complete after deadline release")
	}
	if got, want := output.String(), "deadline-opened"; got != want {
		t.Fatalf("output = %q, want %q", got, want)
	}
}

func TestRunPastAbsoluteDeadlineReleasesImmediately(t *testing.T) {
	now := time.Date(2026, time.January, 2, 3, 4, 0, 0, time.UTC)
	timerCreated := false
	clock := runtimeClock{
		now:      func() time.Time { return now },
		location: time.UTC,
		newTimer: func(time.Duration) (<-chan time.Time, func()) {
			timerCreated = true
			return make(chan time.Time), func() {}
		},
	}
	var output, diagnostics bytes.Buffer
	status := runWithClock([]string{"datetime:2026-01-02T03:03:00"}, strings.NewReader("past"), &output, &diagnostics, clock)
	if status != 0 {
		t.Fatalf("run status = %d, diagnostics = %q", status, diagnostics.String())
	}
	if got, want := output.String(), "past"; got != want {
		t.Fatalf("output = %q, want %q", got, want)
	}
	if timerCreated {
		t.Fatal("past absolute deadline unexpectedly created a timer")
	}
}

func TestRunPastAbsoluteDeadlineReleasesDespiteInitialNonRegularFile(t *testing.T) {
	now := time.Date(2026, time.January, 2, 3, 4, 0, 0, time.UTC)
	clock := runtimeClock{
		now:      func() time.Time { return now },
		location: time.UTC,
	}
	input := strings.NewReader("past")
	var output, diagnostics bytes.Buffer
	status := runWithClock([]string{
		"datetime:2026-01-02T03:03:00",
		"--or",
		"file:" + t.TempDir(),
	}, input, &output, &diagnostics, clock)
	if status != 0 {
		t.Fatalf("run status = %d, diagnostics = %q", status, diagnostics.String())
	}
	if got, want := output.String(), "past"; got != want {
		t.Fatalf("output = %q, want %q", got, want)
	}
	if diagnostics.Len() != 0 {
		t.Fatalf("unexpected diagnostics: %q", diagnostics.String())
	}
}

func TestRunArmsAbsoluteDeadlineBeforeInitialFileProbe(t *testing.T) {
	now := time.Date(2026, time.January, 2, 3, 4, 0, 0, time.UTC)
	var timerCreated bool
	timerStopped := make(chan struct{})
	clock := runtimeClock{
		now:      func() time.Time { return now },
		location: time.UTC,
		newTimer: func(time.Duration) (<-chan time.Time, func()) {
			timerCreated = true
			return make(chan time.Time), func() { close(timerStopped) }
		},
	}
	var output, diagnostics bytes.Buffer
	status := runWithClock([]string{
		"datetime:" + now.Add(time.Minute).Format("2006-01-02T15:04:05"),
		"--or",
		"file:" + t.TempDir(),
	}, strings.NewReader(""), &output, &diagnostics, clock)
	if status != 0 {
		t.Fatalf("run status = %d, diagnostics = %q", status, diagnostics.String())
	}
	if !timerCreated {
		t.Fatal("absolute deadline was not armed before initial file probe")
	}
	select {
	case <-timerStopped:
	case <-time.After(testTimeout):
		t.Fatal("absolute deadline timer was not stopped after empty input")
	}
}

func TestRunZeroDurationStopsFileMonitorBeforeReadiness(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ready")
	input := &firstReadGate{
		data:    []byte("zero-opened"),
		started: make(chan struct{}),
		release: make(chan struct{}),
	}
	output := &lockedBuffer{writeTimes: make(chan time.Time, 1)}
	var diagnostics bytes.Buffer
	status := make(chan int, 1)
	ready := make(chan struct{})
	go func() {
		got, cleanup := executeWithReady([]string{
			"duration:0s",
			"--or",
			"file:" + path,
		}, input, output, &diagnostics, func() {
			if err := os.Mkdir(path, 0o700); err != nil {
				diagnostics.WriteString(err.Error())
			}
			close(ready)
		})
		if cleanup != nil {
			cleanup()
		}
		status <- got
	}()

	select {
	case <-ready:
	case <-time.After(testTimeout):
		t.Fatal("run did not reach readiness")
	}
	select {
	case got := <-status:
		t.Fatalf("run completed before input release with status %d, diagnostics = %q", got, diagnostics.String())
	case <-time.After(100 * time.Millisecond):
	}
	close(input.release)
	select {
	case got := <-status:
		if got != 0 {
			t.Fatalf("run status = %d, diagnostics = %q", got, diagnostics.String())
		}
	case <-time.After(testTimeout):
		t.Fatal("zero-duration run did not complete")
	}
	if got, want := output.String(), "zero-opened"; got != want {
		t.Fatalf("output = %q, want %q", got, want)
	}
}

func TestRunEmptyInputDoesNotWaitForAbsoluteDeadline(t *testing.T) {
	clock := runtimeClock{
		now:      func() time.Time { return time.Date(2026, time.January, 2, 3, 4, 0, 0, time.UTC) },
		location: time.UTC,
		newTimer: func(time.Duration) (<-chan time.Time, func()) {
			return make(chan time.Time), func() {}
		},
	}
	var output, diagnostics bytes.Buffer
	status := make(chan int, 1)
	go func() {
		status <- runWithClock([]string{"datetime:2026-01-03T03:04"}, strings.NewReader(""), &output, &diagnostics, clock)
	}()
	select {
	case got := <-status:
		if got != 0 {
			t.Fatalf("run status = %d, diagnostics = %q", got, diagnostics.String())
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("empty input waited for absolute deadline")
	}
}

func TestRunUsesSmallInitialReadRegionForConfiguredBuffer(t *testing.T) {
	const bufferSize = 64 * 1024
	input := &initialReadSizeReader{readSizes: make(chan int, 1)}
	var output, diagnostics bytes.Buffer
	status := make(chan int, 1)
	go func() {
		status <- run([]string{"duration:1ms", "--buffer-size", "64K"}, input, &output, &diagnostics)
	}()

	var firstReadSize int
	select {
	case firstReadSize = <-input.readSizes:
	case <-time.After(testTimeout):
		t.Fatal("run did not attempt the first read")
	}
	if firstReadSize >= bufferSize {
		t.Fatalf("first read region = %d, want less than configured maximum %d", firstReadSize, bufferSize)
	}

	select {
	case gotStatus := <-status:
		if gotStatus != 0 {
			t.Fatalf("run status = %d, diagnostics = %q", gotStatus, diagnostics.String())
		}
	case <-time.After(testTimeout):
		t.Fatal("run did not complete after releasing configured buffer")
	}
	if got, want := output.String(), "x"; got != want {
		t.Fatalf("output = %q, want %q", got, want)
	}
}

func TestForwardStopsReadingAtConfiguredBufferLimit(t *testing.T) {
	const (
		maxBufferSize = 5000
		readChunkSize = 1024
	)
	input := &configuredBoundedReadReader{
		max:         maxBufferSize,
		chunk:       readChunkSize,
		readStarted: make(chan int, 16),
		full:        make(chan struct{}),
		release:     make(chan struct{}),
	}
	release := input.release
	output := &bytes.Buffer{}
	status := make(chan error, 1)
	go func() {
		delay := time.Hour
		status <- forwardWithFailureAndBuffer(input, output, &delay, release, nil, nil, nil, maxBufferSize)
	}()

	wantReads := (maxBufferSize + readChunkSize - 1) / readChunkSize
	select {
	case <-input.full:
	case <-time.After(testTimeout):
		t.Fatal("forward did not fill the configured buffer")
	}
	for wantRead := 1; wantRead <= wantReads; wantRead++ {
		select {
		case gotRead := <-input.readStarted:
			if gotRead != wantRead {
				t.Fatalf("read sequence = %d, want %d", gotRead, wantRead)
			}
		case <-time.After(time.Second):
			t.Fatalf("forward started only %d of %d reads", wantRead-1, wantReads)
		}
	}
	select {
	case gotRead := <-input.readStarted:
		t.Fatalf("started read %d after reaching configured limit", gotRead)
	case <-time.After(100 * time.Millisecond):
	}

	close(release)
	select {
	case err := <-status:
		if err != nil {
			t.Fatalf("forward returned error: %v", err)
		}
	case <-time.After(testTimeout):
		t.Fatal("forward did not complete after release")
	}
	if got, want := output.Bytes(), input.data; !bytes.Equal(got, want) {
		t.Fatalf("output changed input: got %d bytes, want %d", len(got), len(want))
	}
}

func TestHeldBufferReservesCapacityWithinMaximum(t *testing.T) {
	for _, max := range []int{1, 4095, 4096, 4097, 8191, 8192, 8193} {
		t.Run(strconv.Itoa(max), func(t *testing.T) {
			held := newHeldBuffer(max)
			if got := held.reservedCapacity(); got > max {
				t.Fatalf("initial reserved capacity = %d, want at most %d", got, max)
			}
			if max > initialPreReleaseBufferSize && held.reservedCapacity() >= max {
				t.Fatalf("initial allocation reserved %d bytes for maximum %d", held.reservedCapacity(), max)
			}

			for {
				readBuffer := held.nextReadBuffer()
				if len(readBuffer) == 0 {
					break
				}
				if got := held.reservedCapacity(); got > max {
					t.Fatalf("reserved capacity = %d after growth, want at most %d", got, max)
				}
				if err := held.recordRead(len(readBuffer)); err != nil {
					t.Fatalf("recordRead returned error: %v", err)
				}
			}

			if got := held.reservedCapacity(); got != max {
				t.Fatalf("final reserved capacity = %d, want %d", got, max)
			}
		})
	}
}

func TestHeldBufferDoesNotEagerlyReserveLargeMaximum(t *testing.T) {
	held := newHeldBuffer(1 << 30)
	if got, want := held.reservedCapacity(), initialPreReleaseBufferSize; got != want {
		t.Fatalf("initial reserved capacity = %d, want %d", got, want)
	}
}

func TestHeldBufferWritesChunksInReadOrder(t *testing.T) {
	const maxBufferSize = 4097
	input := make([]byte, maxBufferSize)
	for index := range input {
		input[index] = byte(index)
	}

	held := newHeldBuffer(maxBufferSize)
	for len(input) > 0 {
		readBuffer := held.nextReadBuffer()
		if len(readBuffer) == 0 {
			t.Fatal("held buffer ran out of capacity before all input was recorded")
		}
		n := len(input)
		if n > len(readBuffer) {
			n = len(readBuffer)
		}
		copy(readBuffer[:n], input[:n])
		if err := held.recordRead(n); err != nil {
			t.Fatalf("recordRead returned error: %v", err)
		}
		input = input[n:]
	}

	var output bytes.Buffer
	if err := held.writeTo(&output); err != nil {
		t.Fatalf("writeTo returned error: %v", err)
	}
	want := make([]byte, maxBufferSize)
	for index := range want {
		want[index] = byte(index)
	}
	if !bytes.Equal(output.Bytes(), want) {
		t.Fatalf("held output changed byte order")
	}
	if got := held.reservedCapacity(); got > maxBufferSize {
		t.Fatalf("reserved capacity = %d, want at most %d", got, maxBufferSize)
	}
}

func TestRunReportsSupportedDatetimeSyntaxInInvalidConditionDiagnostic(t *testing.T) {
	var output, diagnostics bytes.Buffer
	if status := run([]string{"term:TERM"}, strings.NewReader("input"), &output, &diagnostics); status == 0 {
		t.Fatal("invalid condition unexpectedly succeeded")
	}
	const want = "invalid release condition \"term:TERM\": want duration:DURATION, datetime:YYYY-MM-DDTHH:MM[:SS], datetime:YYYY-MM-DDTHH:MM:SS[Z|+HH:MM|-HH:MM], signal:USR1, signal:SIGUSR1, signal:USR2, signal:SIGUSR2, or file:PATH\n"
	if got := diagnostics.String(); got != want {
		t.Fatalf("diagnostic = %q, want %q", got, want)
	}
	if output.Len() != 0 {
		t.Fatalf("invalid condition wrote stdout: %q", output.String())
	}
}

func TestForwardReleasesOnInjectedEventBeforeFirstInput(t *testing.T) {
	input := &firstReadGate{
		data:    []byte("event-opened"),
		started: make(chan struct{}),
		release: make(chan struct{}),
	}
	event := make(chan struct{})
	output := &lockedBuffer{writeTimes: make(chan time.Time, 1)}
	status := make(chan error, 1)
	go func() {
		status <- forward(input, output, nil, event)
	}()

	select {
	case <-input.started:
	case <-time.After(time.Second):
		t.Fatal("forward did not attempt the first read")
	}
	close(event)
	select {
	case <-output.writeTimes:
		t.Fatal("output was written before the first read completed")
	default:
	}
	close(input.release)

	select {
	case err := <-status:
		if err != nil {
			t.Fatalf("forward returned error: %v", err)
		}
	case <-time.After(testTimeout):
		t.Fatal("forward did not complete after injected release")
	}
	if got, want := output.String(), "event-opened"; got != want {
		t.Fatalf("output = %q, want %q", got, want)
	}
}

func TestForwardWaitsForInjectedEventAfterDataEOF(t *testing.T) {
	input := eofReader{data: []byte("held until event")}
	event := make(chan struct{})
	output := &lockedBuffer{writeTimes: make(chan time.Time, 1)}
	status := make(chan error, 1)
	go func() {
		status <- forward(input, output, nil, event)
	}()

	select {
	case <-output.writeTimes:
		t.Fatal("EOF released data before the event")
	case <-time.After(100 * time.Millisecond):
	}
	close(event)

	select {
	case err := <-status:
		if err != nil {
			t.Fatalf("forward returned error: %v", err)
		}
	case <-time.After(testTimeout):
		t.Fatal("forward did not complete after injected release")
	}
	if got, want := output.String(), "held until event"; got != want {
		t.Fatalf("output = %q, want %q", got, want)
	}
}

func TestForwardCompletesBufferedReadErrorAfterInjectedRelease(t *testing.T) {
	sentinel := errors.New("buffered input failed")
	input := dataErrorReader{data: []byte("held until error release"), err: sentinel}
	event := make(chan struct{})
	output := &lockedBuffer{writeTimes: make(chan time.Time, 1)}
	status := make(chan error, 1)
	go func() {
		status <- forward(input, output, nil, event)
	}()

	select {
	case <-output.writeTimes:
		t.Fatal("buffered error wrote output before the release event")
	case err := <-status:
		t.Fatalf("buffered error completed before the release event: %v", err)
	case <-time.After(100 * time.Millisecond):
	}

	close(event)
	select {
	case err := <-status:
		if !errors.Is(err, sentinel) {
			t.Fatalf("forward returned error %v, want sentinel %v", err, sentinel)
		}
	case <-time.After(testTimeout):
		t.Fatal("forward did not complete after injected release")
	}
	if got, want := output.String(), string(input.data); got != want {
		t.Fatalf("output = %q, want %q", got, want)
	}
}

func TestForwardExitsOnEmptyEOFWithoutWaitingForInjectedEvent(t *testing.T) {
	event := make(chan struct{})
	var output bytes.Buffer
	status := make(chan error, 1)
	go func() {
		status <- forward(strings.NewReader(""), &output, nil, event)
	}()

	select {
	case err := <-status:
		if err != nil {
			t.Fatalf("forward returned error: %v", err)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("empty EOF waited for injected release")
	}
	if output.Len() != 0 {
		t.Fatalf("empty input produced output: %q", output.String())
	}
}

func TestForwardUsesEarlierOfDurationAndInjectedEvent(t *testing.T) {
	delay := time.Second
	input := eofReader{data: []byte("released")}
	event := make(chan struct{})
	output := &lockedBuffer{writeTimes: make(chan time.Time, 1)}
	status := make(chan error, 1)
	startedAt := time.Now()
	go func() {
		status <- forward(input, output, &delay, event)
	}()

	time.Sleep(50 * time.Millisecond)
	close(event)
	select {
	case wroteAt := <-output.writeTimes:
		if elapsed := wroteAt.Sub(startedAt); elapsed >= delay {
			t.Fatalf("event release took %s, want less than %s", elapsed, delay)
		}
	case <-time.After(testTimeout):
		t.Fatal("event did not release output")
	}
	if err := <-status; err != nil {
		t.Fatalf("forward returned error: %v", err)
	}
}

func TestForwardFlushesWhenTimerIsReadyBeforeReadResult(t *testing.T) {
	delay := 100 * time.Millisecond
	input := &timerReadReader{
		secondStarted: make(chan struct{}),
		secondRelease: make(chan struct{}),
		thirdStarted:  make(chan time.Time, 1),
	}
	output := &lockedBuffer{writeTimes: make(chan time.Time, 2)}
	status := make(chan error, 1)
	go func() {
		status <- forward(input, output, &delay, nil)
	}()

	select {
	case <-input.secondStarted:
	case <-time.After(testTimeout):
		t.Fatal("forward did not start its pre-release read")
	}
	time.Sleep(2 * delay)
	close(input.secondRelease)

	var wroteAt time.Time
	select {
	case wroteAt = <-output.writeTimes:
	case <-time.After(testTimeout):
		t.Fatal("forward did not flush after timer and read became ready")
	}
	select {
	case thirdStartedAt := <-input.thirdStarted:
		if thirdStartedAt.Before(wroteAt) {
			t.Fatalf("started a post-release read at %s before flushing held data at %s", thirdStartedAt, wroteAt)
		}
	case <-time.After(testTimeout):
		t.Fatal("forward did not continue reading after release")
	}
	if err := <-status; err != nil {
		t.Fatalf("forward returned error: %v", err)
	}
	if got, want := output.String(), "abc"; got != want {
		t.Fatalf("output = %q, want %q", got, want)
	}
}

func TestRunRejectsInvalidArguments(t *testing.T) {
	tests := []struct {
		name string
		args []string
	}{
		{name: "missing", args: nil},
		{name: "extra", args: []string{"1s", "2s"}},
		{name: "version extra", args: []string{"--version", "extra"}},
		{name: "invalid", args: []string{"not-a-duration"}},
		{name: "negative", args: []string{"-1s"}},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var output, diagnostics bytes.Buffer
			if status := run(test.args, strings.NewReader("input"), &output, &diagnostics); status == 0 {
				t.Fatal("invalid arguments unexpectedly succeeded")
			}
			if output.Len() != 0 {
				t.Fatalf("invalid arguments wrote stdout: %q", output.String())
			}
			if diagnostics.Len() == 0 {
				t.Fatal("invalid arguments produced no stderr diagnostic")
			}
		})
	}
}

func TestRunReportsInputAndOutputErrors(t *testing.T) {
	inputErr := errors.New("input failed")
	var output, diagnostics bytes.Buffer
	if status := run([]string{"duration:0s"}, errorReader{err: inputErr}, &output, &diagnostics); status == 0 {
		t.Fatal("input error unexpectedly succeeded")
	}
	if output.Len() != 0 {
		t.Fatalf("input error wrote stdout: %q", output.String())
	}
	if !strings.Contains(diagnostics.String(), inputErr.Error()) {
		t.Fatalf("input diagnostic = %q, want %q", diagnostics.String(), inputErr)
	}

	outputErr := errors.New("output failed")
	output.Reset()
	diagnostics.Reset()
	if status := run([]string{"duration:0s"}, strings.NewReader("input"), errorWriter{err: outputErr}, &diagnostics); status == 0 {
		t.Fatal("output error unexpectedly succeeded")
	}
	if !strings.Contains(diagnostics.String(), outputErr.Error()) {
		t.Fatalf("output diagnostic = %q, want %q", diagnostics.String(), outputErr)
	}
}

type firstReadGate struct {
	data    []byte
	started chan struct{}
	release chan struct{}
	once    sync.Once
}

func (r *firstReadGate) Read(p []byte) (int, error) {
	r.once.Do(func() { close(r.started) })
	<-r.release
	n := copy(p, r.data)
	return n, io.EOF
}

type eofReader struct {
	data []byte
}

func (r eofReader) Read(p []byte) (int, error) {
	n := copy(p, r.data)
	return n, io.EOF
}

type dataErrorReader struct {
	data []byte
	err  error
}

func (r dataErrorReader) Read(p []byte) (int, error) {
	return copy(p, r.data), r.err
}

type backpressureReader struct {
	chunks         [][]byte
	firstRead      chan time.Time
	secondStarted  chan time.Time
	blockedRead    chan time.Time
	releaseBlocked chan struct{}
	reads          int
}

type boundedReadReader struct {
	calls   chan int
	release chan struct{}
	reads   int
	data    []byte
}

type fragmentedReadReader struct {
	readStarted chan int
	release     chan struct{}
	releaseOnce sync.Once
	reads       int
	data        []byte
}

func (r *fragmentedReadReader) Read(p []byte) (int, error) {
	r.reads++
	r.readStarted <- r.reads
	if len(r.data) < preReleaseBufferSize {
		readSize := len(p)
		if readSize > 1024 {
			readSize = 1024
		}
		if remaining := preReleaseBufferSize - len(r.data); readSize > remaining {
			readSize = remaining
		}
		for i := 0; i < readSize; i++ {
			p[i] = byte(r.reads)
		}
		r.data = append(r.data, p[:readSize]...)
		return readSize, nil
	}
	<-r.release
	return 0, io.EOF
}

func (r *fragmentedReadReader) unblock() {
	r.releaseOnce.Do(func() { close(r.release) })
}

func (r *boundedReadReader) Read(p []byte) (int, error) {
	r.reads++
	switch r.reads {
	case 1:
		p[0] = 'a'
		r.data = append(r.data, p[0])
		r.calls <- 1
		return 1, nil
	case 2:
		n := fillLimit(p, 'b', readBufferSize)
		r.data = append(r.data, p[:n]...)
		r.calls <- n
		return n, nil
	case 3:
		n := fillLimit(p, 'c', readBufferSize)
		r.data = append(r.data, p[:n]...)
		r.calls <- n
		return n, nil
	default:
		<-r.release
		return 0, io.EOF
	}
}

func fill(p []byte, value byte) int {
	for i := range p {
		p[i] = value
	}
	return len(p)
}

func fillLimit(p []byte, value byte, limit int) int {
	if len(p) > limit {
		p = p[:limit]
	}
	return fill(p, value)
}

type postReleaseReader struct {
	release chan struct{}
	reads   int
}

type timerReadReader struct {
	secondStarted chan struct{}
	secondRelease chan struct{}
	thirdStarted  chan time.Time
	reads         int
}

func (r *timerReadReader) Read(p []byte) (int, error) {
	r.reads++
	switch r.reads {
	case 1:
		p[0] = 'a'
		return 1, nil
	case 2:
		close(r.secondStarted)
		<-r.secondRelease
		p[0] = 'b'
		return 1, nil
	case 3:
		r.thirdStarted <- time.Now()
		p[0] = 'c'
		return 1, io.EOF
	default:
		return 0, io.EOF
	}
}

func (r *postReleaseReader) Read(p []byte) (int, error) {
	r.reads++
	switch r.reads {
	case 1:
		return copy(p, "first"), nil
	case 2:
		<-r.release
		return copy(p, "second"), io.EOF
	default:
		return 0, io.EOF
	}
}

func (r *backpressureReader) Read(p []byte) (int, error) {
	switch {
	case r.reads == 0:
		r.reads++
		r.firstRead <- time.Now()
		return copy(p, r.chunks[0]), nil
	case r.reads == 1:
		r.reads++
		r.secondStarted <- time.Now()
		return copy(p, r.chunks[1]), nil
	case r.reads == 2:
		r.reads++
		r.blockedRead <- time.Now()
		<-r.releaseBlocked
		return copy(p, r.chunks[2]), io.EOF
	default:
		return 0, io.EOF
	}
}

type errorReader struct {
	err error
}

func (r errorReader) Read([]byte) (int, error) {
	return 0, r.err
}

type trackingReader struct {
	reads int
}

func (r *trackingReader) Read([]byte) (int, error) {
	r.reads++
	return 0, errors.New("stdin should not be read")
}

type initialReadSizeReader struct {
	readSizes chan int
}

func (r *initialReadSizeReader) Read(p []byte) (int, error) {
	r.readSizes <- len(p)
	p[0] = 'x'
	return 1, io.EOF
}

type configuredBoundedReadReader struct {
	max         int
	chunk       int
	readStarted chan int
	full        chan struct{}
	release     chan struct{}
	reads       int
	data        []byte
	once        sync.Once
}

func (r *configuredBoundedReadReader) Read(p []byte) (int, error) {
	r.reads++
	r.readStarted <- r.reads
	if len(r.data) < r.max {
		readSize := len(p)
		if readSize > r.chunk {
			readSize = r.chunk
		}
		if remaining := r.max - len(r.data); readSize > remaining {
			readSize = remaining
		}
		for index := 0; index < readSize; index++ {
			p[index] = byte(r.reads)
		}
		r.data = append(r.data, p[:readSize]...)
		if len(r.data) == r.max {
			r.once.Do(func() { close(r.full) })
		}
		return readSize, nil
	}
	<-r.release
	return 0, io.EOF
}

type errorWriter struct {
	err error
}

func (w errorWriter) Write([]byte) (int, error) {
	return 0, w.err
}

type lockedBuffer struct {
	mu         sync.Mutex
	writeTimes chan time.Time
	bytes.Buffer
}

func (b *lockedBuffer) Write(data []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	n, err := b.Buffer.Write(data)
	if n > 0 && b.writeTimes != nil {
		// Chunked pre-release storage can flush with multiple writes. The
		// notification is only a timing observation, so never let a full test
		// channel block the stream writer.
		select {
		case b.writeTimes <- time.Now():
		default:
		}
	}
	return n, err
}

func (b *lockedBuffer) ReadFrom(input io.Reader) (int64, error) {
	buffer := make([]byte, readBufferSize)
	var total int64
	for {
		n, readErr := input.Read(buffer)
		if n > 0 {
			written, writeErr := b.Write(buffer[:n])
			total += int64(written)
			if writeErr != nil {
				return total, writeErr
			}
			if written != n {
				return total, io.ErrShortWrite
			}
		}
		if readErr != nil {
			if readErr == io.EOF {
				return total, nil
			}
			return total, readErr
		}
		if n == 0 {
			return total, io.ErrNoProgress
		}
	}
}

func (b *lockedBuffer) Len() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.Buffer.Len()
}

func (b *lockedBuffer) Bytes() []byte {
	b.mu.Lock()
	defer b.mu.Unlock()
	return bytes.Clone(b.Buffer.Bytes())
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.Buffer.String()
}
