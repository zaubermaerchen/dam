package main

// This file verifies dam's argument validation and delayed stdin forwarding.

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
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

Hold pipeline output until a release condition is met.

Arguments:
  CONDITION
        A condition is one of:
          duration:DURATION
              A Go duration (such as 500ms, 3s, or 2m) starts after the
              first non-empty stdin read (except 0s, which is immediate).
              Multiple duration conditions share that starting read.
          datetime:YYYY-MM-DDTHH:MM[:SS]
              An absolute local datetime monitored from startup. Multiple
              datetime conditions are allowed.
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

Notes:
        Equivalent duration values and resolved datetime values share one
        latched event. Time and file monitors stop after release or empty
        stdin reaches EOF.

  -h, --help
        Show this help and exit.

  --version
        Show version and exit.
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
		"Equivalent duration values",
		"equivalent datetime values",
		"monitoring stop",
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
	return string(content)
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

func TestParseConfigAcceptsDurationAndRepeatableSignalsInAnyOrder(t *testing.T) {
	config, err := parseConfig([]string{
		"signal:USR1",
		"--or",
		"duration:250ms",
		"--or",
		"signal:SIGUSR1",
	})
	if err != nil {
		t.Fatalf("parseConfig returned error: %v", err)
	}
	if config.delay == nil || *config.delay != 250*time.Millisecond {
		t.Fatalf("delay = %v, want 250ms", config.delay)
	}
	if got, want := config.signals, []string{"SIGUSR1", "SIGUSR1"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("signals = %v, want %v", got, want)
	}
}

func TestParseConfigAcceptsSignalWithoutDuration(t *testing.T) {
	config, err := parseConfig([]string{"signal:SIGUSR1"})
	if err != nil {
		t.Fatalf("parseConfig returned error: %v", err)
	}
	if config.delay != nil {
		t.Fatalf("delay = %v, want nil", config.delay)
	}
	if got, want := config.signals, []string{"SIGUSR1"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("signals = %v, want %v", got, want)
	}
}

func TestParseConfigAcceptsAbsoluteLocalDeadline(t *testing.T) {
	location := time.FixedZone("test", 9*60*60)
	config, err := parseConfigAt([]string{"datetime:2026-12-31T23:59"}, location)
	if err != nil {
		t.Fatalf("parseConfigAt returned error: %v", err)
	}
	if config.delay != nil {
		t.Fatalf("delay = %v, want nil", config.delay)
	}
	if config.deadline == nil {
		t.Fatal("deadline = nil, want absolute deadline")
	}
	want := time.Date(2026, time.December, 31, 23, 59, 0, 0, location)
	if !config.deadline.Equal(want) {
		t.Fatalf("deadline = %s, want %s", config.deadline, want)
	}
}

func TestParseConfigAcceptsAbsoluteDeadlineWithSeconds(t *testing.T) {
	config, err := parseConfigAt([]string{"datetime:2026-12-31T23:59:07"}, time.UTC)
	if err != nil {
		t.Fatalf("parseConfigAt returned error: %v", err)
	}
	if config.deadline == nil {
		t.Fatal("deadline = nil, want absolute deadline")
	}
	if got, want := config.deadline.Second(), 7; got != want {
		t.Fatalf("deadline second = %d, want %d", got, want)
	}
}

func TestParseConfigAcceptsAbsoluteDeadlineYearBoundaries(t *testing.T) {
	for _, value := range []string{"0001-01-01T00:00", "9999-12-31T23:59:59"} {
		t.Run(value, func(t *testing.T) {
			config, err := parseConfigAt([]string{"datetime:" + value}, time.UTC)
			if err != nil {
				t.Fatalf("parseConfigAt returned error: %v", err)
			}
			if config.deadline == nil {
				t.Fatal("deadline = nil, want absolute deadline")
			}
		})
	}
}

func TestParseConfigRejectsMalformedAbsoluteDeadlines(t *testing.T) {
	for _, value := range []string{
		"0000-01-01T00:00",
		"2026-02-29T12:00",
		"2026-12-31T24:00",
		"2026-12-31T23:59:60",
		"2026-12-31t23:59",
		"2026-12-31 23:59",
		"2026-12-31T23:xx",
		"2026-12-31T23:59.1",
		"2026-12-31T23:59Z",
		"2026-12-31T23:59+09:00",
	} {
		t.Run(value, func(t *testing.T) {
			if _, err := parseConfigAt([]string{"datetime:" + value}, time.UTC); err == nil {
				t.Fatal("parseConfigAt unexpectedly succeeded")
			}
		})
	}
}

func TestParseConfigAcceptsMultipleKindsOfTimedConditions(t *testing.T) {
	config, err := parseConfigAt([]string{
		"duration:1s",
		"--or",
		"datetime:2026-12-31T23:59",
		"--or=datetime:2027-01-01T00:00",
	}, time.UTC)
	if err != nil {
		t.Fatalf("parseConfigAt returned error: %v", err)
	}
	if len(config.groups) != 3 {
		t.Fatalf("groups = %#v, want three alternatives", config.groups)
	}
}

func TestParseAbsoluteDeadlineUsesEarlierInstantForDSTOverlap(t *testing.T) {
	location, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Skipf("load timezone: %v", err)
	}
	got, err := parseAbsoluteDeadline("2024-11-03T01:30", location)
	if err != nil {
		t.Fatalf("parseAbsoluteDeadline returned error: %v", err)
	}
	want := time.Date(2024, time.November, 3, 1, 30, 0, 0, time.FixedZone("EDT", -4*60*60))
	if !got.Equal(want) {
		t.Fatalf("deadline = %s (%s), want %s (%s)", got, got.UTC(), want, want.UTC())
	}
}

func TestEarliestLocalInstantFindsShortLivedHistoricalOffset(t *testing.T) {
	location, err := time.LoadLocationFromTZData("Synthetic/Short", shortLivedOffsetTZif(t))
	if err != nil {
		t.Fatalf("load synthetic timezone: %v", err)
	}
	parsed := time.Unix(30, 0).In(location)
	if !sameLocalDateTime(parsed, 1970, time.January, 1, 0, 0, 0) {
		t.Fatalf("later occurrence = %s, want 1970-01-01T00:00:00", parsed)
	}

	got := earliestLocalInstant(parsed, 1970, time.January, 1, 0, 0, 0, location)
	want := time.Unix(0, 0).In(location)
	if !got.Equal(want) {
		t.Fatalf("earliest occurrence = %s (%s), want %s (%s)", got, got.UTC(), want, want.UTC())
	}
}

func TestParseAbsoluteDeadlineHandlesTransitionAtZeroTime(t *testing.T) {
	location, err := time.LoadLocationFromTZData("Synthetic/Zero", zeroTimeTransitionTZif(t))
	if err != nil {
		t.Fatalf("load synthetic timezone: %v", err)
	}

	got, err := parseAbsoluteDeadline("0001-01-01T00:00:00", location)
	if err != nil {
		t.Fatalf("parseAbsoluteDeadline returned error: %v", err)
	}
	want := time.Date(1, time.January, 1, 0, 0, -30, 0, time.UTC).In(location)
	if !got.Equal(want) {
		t.Fatalf("earliest occurrence = %s (%s), want %s (%s)", got, got.UTC(), want, want.UTC())
	}
}

func shortLivedOffsetTZif(t *testing.T) []byte {
	t.Helper()
	var data bytes.Buffer
	data.WriteString("TZif")
	data.Write(make([]byte, 16))
	for _, count := range []uint32{0, 0, 0, 3, 4, 8} {
		if err := binary.Write(&data, binary.BigEndian, count); err != nil {
			t.Fatalf("write TZif header: %v", err)
		}
	}
	for _, transition := range []int32{-20, 10, 20} {
		if err := binary.Write(&data, binary.BigEndian, transition); err != nil {
			t.Fatalf("write TZif transition: %v", err)
		}
	}
	data.Write([]byte{1, 2, 3})
	for _, zone := range []struct {
		offset int32
		name   byte
	}{
		{offset: -7200, name: 0},
		{offset: 0, name: 2},
		{offset: 36000, name: 4},
		{offset: -30, name: 6},
	} {
		if err := binary.Write(&data, binary.BigEndian, zone.offset); err != nil {
			t.Fatalf("write TZif offset: %v", err)
		}
		data.WriteByte(0)
		data.WriteByte(zone.name)
	}
	data.WriteString("D\x00A\x00B\x00C\x00")
	return data.Bytes()
}

func zeroTimeTransitionTZif(t *testing.T) []byte {
	t.Helper()
	var data bytes.Buffer
	writeTZifHeader(t, &data, '2', 0, 1, 2)
	writeTZifZone(t, &data, 0, 0)
	data.WriteString("X\x00")

	writeTZifHeader(t, &data, '2', 1, 2, 4)
	if err := binary.Write(&data, binary.BigEndian, int64(-62135596800)); err != nil {
		t.Fatalf("write TZif transition: %v", err)
	}
	data.WriteByte(1)
	writeTZifZone(t, &data, 30, 0)
	writeTZifZone(t, &data, 0, 2)
	data.WriteString("A\x00B\x00")
	data.WriteString("\n\n")
	return data.Bytes()
}

func writeTZifHeader(t *testing.T, data *bytes.Buffer, version byte, transitions, zones, names uint32) {
	t.Helper()
	data.WriteString("TZif")
	data.WriteByte(version)
	data.Write(make([]byte, 15))
	for _, count := range []uint32{0, 0, 0, transitions, zones, names} {
		if err := binary.Write(data, binary.BigEndian, count); err != nil {
			t.Fatalf("write TZif header: %v", err)
		}
	}
}

func writeTZifZone(t *testing.T, data *bytes.Buffer, offset int32, name byte) {
	t.Helper()
	if err := binary.Write(data, binary.BigEndian, offset); err != nil {
		t.Fatalf("write TZif offset: %v", err)
	}
	data.WriteByte(0)
	data.WriteByte(name)
}

func TestParseAbsoluteDeadlineRejectsDSTGap(t *testing.T) {
	location, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Skipf("load timezone: %v", err)
	}
	if _, err := parseAbsoluteDeadline("2024-03-10T02:30", location); err == nil {
		t.Fatal("parseAbsoluteDeadline unexpectedly accepted a DST gap")
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

func TestRunPastAbsoluteDeadlineDoesNotOverrideInitialFileFatal(t *testing.T) {
	now := time.Date(2026, time.January, 2, 3, 4, 0, 0, time.UTC)
	clock := runtimeClock{
		now:      func() time.Time { return now },
		location: time.UTC,
	}
	input := &trackingReader{}
	var output, diagnostics bytes.Buffer
	status := runWithClock([]string{
		"datetime:2026-01-02T03:03:00",
		"--or",
		"file:" + t.TempDir(),
	}, input, &output, &diagnostics, clock)
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
	}, &trackingReader{}, &output, &diagnostics, clock)
	if status == 0 {
		t.Fatal("directory release condition unexpectedly succeeded")
	}
	if !timerCreated {
		t.Fatal("absolute deadline was not armed before initial file probe")
	}
	select {
	case <-timerStopped:
	case <-time.After(testTimeout):
		t.Fatal("absolute deadline timer was not stopped on initial probe failure")
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

func TestParseConfigAcceptsUSR2AliasesAndPreservesOrder(t *testing.T) {
	config, err := parseConfig([]string{
		"signal:USR2",
		"--or",
		"duration:250ms",
		"--or",
		"signal:SIGUSR2",
	})
	if err != nil {
		t.Fatalf("parseConfig returned error: %v", err)
	}
	if config.delay == nil || *config.delay != 250*time.Millisecond {
		t.Fatalf("delay = %v, want 250ms", config.delay)
	}
	if got, want := config.signals, []string{"SIGUSR2", "SIGUSR2"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("signals = %v, want %v", got, want)
	}
}

func TestParseConfigDefaultsBufferSize(t *testing.T) {
	config, err := parseConfig([]string{"duration:1s"})
	if err != nil {
		t.Fatalf("parseConfig returned error: %v", err)
	}
	if got, want := config.bufferSize, preReleaseBufferSize; got != want {
		t.Fatalf("buffer size = %d, want default %d", got, want)
	}
}

func TestParseConfigAcceptsBufferSizeFormsAndBinaryUnits(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want int
	}{
		{name: "bytes separated", args: []string{"duration:1s", "--buffer-size", "123"}, want: 123},
		{name: "bytes equals", args: []string{"--buffer-size=123", "duration:1s"}, want: 123},
		{name: "upper kilobytes", args: []string{"duration:1s", "--buffer-size", "2K"}, want: 2 * 1024},
		{name: "lower kilobytes", args: []string{"duration:1s", "--buffer-size=1k"}, want: 1024},
		{name: "upper megabytes", args: []string{"duration:1s", "--buffer-size=3M"}, want: 3 * 1024 * 1024},
		{name: "lower megabytes", args: []string{"--buffer-size", "4m", "duration:1s"}, want: 4 * 1024 * 1024},
		{name: "upper gigabytes", args: []string{"duration:1s", "--buffer-size=1G"}, want: 1024 * 1024 * 1024},
		{name: "lower gigabytes", args: []string{"--buffer-size", "1g", "duration:1s"}, want: 1024 * 1024 * 1024},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			config, err := parseConfig(test.args)
			if err != nil {
				t.Fatalf("parseConfig returned error: %v", err)
			}
			if config.bufferSize != test.want {
				t.Fatalf("buffer size = %d, want %d", config.bufferSize, test.want)
			}
		})
	}
}

func TestParseConfigRejectsInvalidBufferSizes(t *testing.T) {
	for _, value := range []string{
		"",
		"0",
		"-1",
		"+1",
		"1.5",
		"1KB",
		"1KiB",
		"1KK",
		"1T",
		"18446744073709551615",
		"18446744073709551615G",
	} {
		t.Run(value, func(t *testing.T) {
			args := []string{"duration:1s", "--buffer-size", value}
			if _, err := parseConfig(args); err == nil {
				t.Fatal("parseConfig unexpectedly succeeded")
			}
		})
	}

	for _, args := range [][]string{
		{"duration:1s", "--buffer-size"},
		{"duration:1s", "--buffer-size="},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			if _, err := parseConfig(args); err == nil {
				t.Fatal("parseConfig unexpectedly succeeded")
			}
		})
	}
}

func TestParseConfigBufferSizeAloneIsNotAReleaseCondition(t *testing.T) {
	if _, err := parseConfig([]string{"--buffer-size", "1K"}); err == nil {
		t.Fatal("buffer-size without a release condition unexpectedly succeeded")
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

func TestParseBufferSize32BitBoundaries(t *testing.T) {
	if strconv.IntSize != 32 {
		t.Skip("32-bit integer boundary test")
	}

	for _, value := range []string{"2147483648", "2G", "3G"} {
		t.Run(value, func(t *testing.T) {
			if _, err := parseBufferSize(value); err == nil {
				t.Fatalf("parseBufferSize(%q) unexpectedly succeeded", value)
			}
		})
	}
	if got, err := parseBufferSize("2147483647"); err != nil || got != 2147483647 {
		t.Fatalf("parseBufferSize(2147483647) = %d, %v", got, err)
	}
}

func TestParseConfigRejectsInvalidUSR2Conditions(t *testing.T) {
	for _, value := range []string{
		"signal:usr2",
		"signal:SigUSR2",
		"signal:SIGUSR2:extra",
	} {
		t.Run(value, func(t *testing.T) {
			if _, err := parseConfig([]string{value}); err == nil {
				t.Fatal("parseConfig unexpectedly succeeded")
			}
		})
	}
}

func TestParseConfigDistinguishesAdditionalPositionalArguments(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want string
	}{
		{
			name: "second duration",
			args: []string{"duration:1s", "duration:2s"},
			want: `unexpected argument "duration:2s"`,
		},
		{
			name: "unexpected argument",
			args: []string{"duration:1s", "foo"},
			want: `unexpected argument "foo"`,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := parseConfig(test.args)
			if err == nil {
				t.Fatal("parseConfig unexpectedly succeeded")
			}
			if got := err.Error(); !strings.Contains(got, test.want) {
				t.Fatalf("parseConfig error = %q, want substring %q", got, test.want)
			}
		})
	}
}

func TestParseConfigRejectsInvalidReleaseConditions(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want string
	}{
		{name: "missing all", args: nil, want: "usage: dam CONDITION [--or CONDITION]... [--buffer-size SIZE]"},
		{name: "duplicate duration", args: []string{"duration:1s", "duration:2s"}},
		{name: "missing release value", args: []string{"--release-on"}},
		{name: "empty release value", args: []string{"--release-on="}},
		{name: "missing source", args: []string{"signal:"}},
		{name: "unknown type", args: []string{"term:TERM"}},
		{name: "unknown signal", args: []string{"signal:TERM"}},
		{name: "lowercase type", args: []string{"Signal:USR1"}},
		{name: "lowercase source", args: []string{"signal:usr1"}},
		{name: "extra separator", args: []string{"signal:USR1:extra"}},
		{name: "version with release", args: []string{"--version", "--release-on", "signal:USR1"}},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := parseConfig(test.args)
			if err == nil {
				t.Fatal("parseConfig unexpectedly succeeded")
			}
			if test.want != "" && err.Error() != test.want {
				t.Fatalf("parseConfig error = %q, want %q", err, test.want)
			}
		})
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
