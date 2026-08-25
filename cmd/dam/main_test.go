package main

// This file verifies dam's argument validation and delayed stdin forwarding.

import (
	"bytes"
	"errors"
	"io"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

const (
	readBufferSize = 32 * 1024
	testTimeout    = 5 * time.Second
)

func TestRunPreservesBinaryInputWithZeroDelay(t *testing.T) {
	input := make([]byte, 256*3)
	for i := range input {
		input[i] = byte(i)
	}

	var output bytes.Buffer
	var diagnostics bytes.Buffer
	if status := run([]string{"0s"}, bytes.NewReader(input), &output, &diagnostics); status != 0 {
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

func TestRunPreservesDelayedBinaryInput(t *testing.T) {
	const delay = 100 * time.Millisecond
	input := eofReader{data: []byte{0x00, 0xff, 0x01, 0xfe, 0x7f}}
	output := &lockedBuffer{writeTimes: make(chan time.Time, 1)}
	var diagnostics bytes.Buffer
	startedAt := time.Now()
	status := make(chan int, 1)
	go func() {
		status <- run([]string{delay.String()}, input, output, &diagnostics)
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
		status <- run([]string{delay.String()}, input, output, &diagnostics)
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
		status <- run([]string{delay.String()}, eofReader{data: []byte("before eof")}, output, &diagnostics)
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
		status <- run([]string{"1h"}, strings.NewReader(""), &output, &diagnostics)
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
		status <- run([]string{delay.String()}, input, output, &diagnostics)
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
		status <- run([]string{delay.String()}, input, output, &diagnostics)
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
		status <- run([]string{delay.String()}, input, output, &diagnostics)
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
		status <- run([]string{delay.String()}, input, output, &diagnostics)
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
		"--release-on=signal:USR1",
		"250ms",
		"--release-on",
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
	config, err := parseConfig([]string{"--release-on", "signal:SIGUSR1"})
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

func TestParseConfigRejectsInvalidReleaseConditions(t *testing.T) {
	tests := []struct {
		name string
		args []string
	}{
		{name: "missing all", args: nil},
		{name: "duplicate duration", args: []string{"1s", "2s"}},
		{name: "missing release value", args: []string{"--release-on"}},
		{name: "empty release value", args: []string{"--release-on="}},
		{name: "missing source", args: []string{"--release-on", "signal:"}},
		{name: "unknown type", args: []string{"--release-on", "term:TERM"}},
		{name: "unknown signal", args: []string{"--release-on", "signal:TERM"}},
		{name: "lowercase type", args: []string{"--release-on", "Signal:USR1"}},
		{name: "lowercase source", args: []string{"--release-on", "signal:usr1"}},
		{name: "extra separator", args: []string{"--release-on", "signal:USR1:extra"}},
		{name: "version with release", args: []string{"--version", "--release-on", "signal:USR1"}},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := parseConfig(test.args); err == nil {
				t.Fatal("parseConfig unexpectedly succeeded")
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
	if status := run([]string{"0s"}, errorReader{err: inputErr}, &output, &diagnostics); status == 0 {
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
	if status := run([]string{"0s"}, strings.NewReader("input"), errorWriter{err: outputErr}, &diagnostics); status == 0 {
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
		b.writeTimes <- time.Now()
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
