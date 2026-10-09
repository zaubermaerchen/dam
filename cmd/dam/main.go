package main

// This file implements version reporting, release coordination, and delayed
// stdin-to-stdout forwarding.

import (
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"time"

	"github.com/zaubermaerchen/dam/internal/condition"
	"github.com/zaubermaerchen/dam/internal/events"
)

// Keep the initial allocation small so large configured limits are only paid
// for when input actually fills the pre-release buffer.
const initialPreReleaseBufferSize = 4 * 1024

const helpText = `Usage:
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
        Conditions joined by the exact ASCII separator " && " inside one
        argument form an AND group. Quote the entire group. Every member is
        latched once satisfied. Use --or between alternative conditions.
        File paths preserve leading/trailing whitespace without trimming.
        Without that separator, "&&" is literal path text without a warning:
          "file:a1 &&file:a2" -> one path "a1 &&file:a2"
          "file:a1&& file:a2" -> one path "a1&& file:a2"
          "file:a1  && file:a2" -> AND paths "a1 " and "a2"
        The exact " && " separator cannot be literal text in a file path.

Options:
  --or CONDITION
        Make CONDITION an alternative to the preceding condition. May be
        written as --or=CONDITION.

  --buffer-size SIZE
        Set the maximum pre-release buffer size (default: 64K).
        SIZE is a positive byte count or a binary K/k, M/m, or G/g value.
        Also accepted as --buffer-size=SIZE.

  --events-fd N
        Emit release-selected and stream-open JSONL events to an already
        nonblocking pipe, FIFO, or socket descriptor N (Windows: NOWAIT pipe).
        Also accepted as --events-fd=N. Invalid descriptors fail at startup;
        later transport failures disable events and attempt one warning.

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

var version = "dev"

var errReleaseFailureChannelClosed = errors.New("internal error: release failure channel closed")

func main() {
	// Keep configured signals registered until os.Exit so they remain consumed
	// during normal CLI shutdown. The run wrapper cleans up long-lived unit tests.
	var waitWarning func()
	status, _ := executeWithClockAndExitWait(os.Args[1:], os.Stdin, os.Stdout, os.Stderr, nil, defaultRuntimeClock(), &waitWarning)
	if waitWarning != nil {
		// Only process exit spends the warning budget; stream transfer and
		// cleanup remain independent of a blocked stderr.
		waitWarning()
	}
	os.Exit(status)
}

func execute(args []string, input io.Reader, output, diagnostics io.Writer) (int, func()) {
	return executeWithClock(args, input, output, diagnostics, nil, defaultRuntimeClock())
}

// runtimeClock contains the process-wide time dependencies. Keeping these
// dependencies at the execution boundary lets tests exercise absolute
// deadlines without waiting on wall-clock time, while production uses the
// normal time package behavior.
type runtimeClock struct {
	now      func() time.Time
	location *time.Location
	newTimer func(time.Duration) (<-chan time.Time, func())
}

func defaultRuntimeClock() runtimeClock {
	return runtimeClock{
		now:      time.Now,
		location: time.Local,
		newTimer: func(delay time.Duration) (<-chan time.Time, func()) {
			timer := time.NewTimer(delay)
			return timer.C, func() { timer.Stop() }
		},
	}
}

func (clock runtimeClock) normalized() runtimeClock {
	if clock.now == nil {
		clock.now = time.Now
	}
	if clock.location == nil {
		clock.location = time.Local
		if clock.location == nil {
			clock.location = time.UTC
		}
	}
	if clock.newTimer == nil {
		clock.newTimer = defaultRuntimeClock().newTimer
	}
	return clock
}

func executeWithClock(args []string, input io.Reader, output, diagnostics io.Writer, ready func(), clock runtimeClock) (int, func()) {
	return executeWithClockAndExitWait(args, input, output, diagnostics, ready, clock, nil)
}

// executeWithClockAndExitWait exposes only warning completion to main so gate
// cleanup can remain deferred until process exit, preserving signal handling.
func executeWithClockAndExitWait(args []string, input io.Reader, output, diagnostics io.Writer, ready func(), clock runtimeClock, exitWait *func()) (int, func()) {
	if slices.Contains(args, "-h") || slices.Contains(args, "--help") {
		if err := writeAll(output, []byte(helpText)); err != nil {
			writeDiagnostic(diagnostics, err)
			return 1, nil
		}
		return 0, nil
	}

	if describeRequested(args) {
		if len(args) != 1 {
			writeDiagnostic(diagnostics, fmt.Errorf("--describe must be specified alone"))
			return 1, nil
		}
		if err := printDescription(output); err != nil {
			writeDiagnostic(diagnostics, err)
			return 1, nil
		}
		return 0, nil
	}

	if len(args) == 1 && args[0] == "--version" {
		if err := writeAll(output, []byte(fmt.Sprintf("dam %s\n", version))); err != nil {
			writeDiagnostic(diagnostics, err)
			return 1, nil
		}
		return 0, nil
	}

	clock = clock.normalized()
	config, err := parseConfigAt(args, clock.location)
	if err != nil {
		writeDiagnostic(diagnostics, err)
		return 1, nil
	}
	eventSink, err := events.New(config.eventsFD, diagnostics)
	if err != nil {
		writeDiagnostic(diagnostics, err)
		return 2, nil
	}

	if exitWait != nil {
		*exitWait = eventSink.WaitWarning
	}

	engine, err := condition.New(config.conditionPlan(), condition.Options{
		Now:      clock.now,
		NewTimer: clock.newTimer,
	})
	if err != nil {
		writeDiagnostic(diagnostics, err)
		eventSink.Close()
		return 1, nil
	}
	gate := newReleaseGate(engine, eventSink)
	cleanup := func() {
		gate.close()
		eventSink.Close()
	}
	if err := engine.Start(); err != nil {
		writeDiagnostic(diagnostics, err)
		cleanup()
		return 1, cleanup
	}
	// A condition can be selected during startup (notably duration:0s or an
	// already-regular file). Commit that transition before the first stdin
	// read starts so a blocked or empty input cannot observe a half-emitted
	// lifecycle transition.
	select {
	case <-gate.selected():
		if err := gate.commitOpen(); err != nil {
			writeDiagnostic(diagnostics, err)
			cleanup()
			return 1, cleanup
		}
	default:
	}
	if ready != nil {
		ready()
	}

	if err := forwardWithReleaseAndBuffer(input, output, gate.selected(), engine.Failures(), gate.commitOpen, gate.completeEmpty, config.bufferSize, engine.StartDurations); err != nil {
		writeDiagnostic(diagnostics, err)
		cleanup()
		return 1, cleanup
	}
	return 0, cleanup
}

// startDuration must be nonnil; callers without duration monitoring pass a no-op.
func forwardWithReleaseAndBuffer(input io.Reader, output io.Writer, release <-chan struct{}, failures <-chan error, open, completeEmpty func() error, bufferSize int, startDuration func() error) error {
	held := newHeldBuffer(bufferSize)
	firstReadBuffer := held.nextReadBuffer()
	firstResults := make(chan readResult, 1)
	startRead(input, firstReadBuffer, firstResults)

	for {
		select {
		case err, ok := <-failures:
			if !ok {
				return errReleaseFailureChannelClosed
			}
			if err != nil {
				return err
			}
		case <-release:
			if err := commitOpen(open, failures); err != nil {
				return err
			}
			// A signal may arrive before the first read has returned. The read
			// still owns the input ordering, so wait for its result before
			// forwarding the now-open stream.
			return forwardReadResultWithCompletion(input, output, firstReadBuffer, <-firstResults, completeEmpty)
		case result := <-firstResults:
			if err := failureReady(failures); err != nil {
				return err
			}
			if result.n == 0 {
				if result.err == io.EOF {
					// Prefer a root that became selected while the first read
					// completed over treating the same boundary as empty input.
					// Otherwise completeEmpty may close the condition engine
					// before the runtime emits the OPEN lifecycle events.
					select {
					case <-release:
						if err := commitOpen(open, failures); err != nil {
							return err
						}
					default:
					}
					return completeEmptyInput(completeEmpty, failures)
				}
				if result.err != nil {
					return result.err
				}
				startRead(input, firstReadBuffer, firstResults)
				continue
			}

			// Prefer a release that was already delivered over starting a new
			// duration window at the same boundary.
			select {
			case <-release:
				if err := commitOpen(open, failures); err != nil {
					return err
				}
				return forwardReadResultWithCompletion(input, output, firstReadBuffer, result, completeEmpty)
			default:
			}
			if err := failureReady(failures); err != nil {
				return err
			}

			if err := held.recordRead(result.n); err != nil {
				return err
			}
			if err := startDuration(); err != nil {
				return err
			}
			if result.err != nil {
				return forwardHeldBufferUntilReleaseWithFailure(output, release, failures, open, held, result.err)
			}
			return forwardDelayedBufferWithFailure(input, output, release, failures, open, completeEmpty, held)
		}
	}
}

// heldBuffer stores pre-release input in separately allocated chunks. Keeping
// old chunks alive while growing avoids the transient old-plus-new allocation
// that a contiguous slice requires, while reserving no more than max bytes in
// total.
type heldBuffer struct {
	chunks   [][]byte
	used     []int
	max      int
	reserved int
}

func newHeldBuffer(max int) *heldBuffer {
	if max < 0 {
		max = 0
	}
	held := &heldBuffer{max: max}
	held.grow()
	return held
}

func (held *heldBuffer) grow() bool {
	if held == nil || held.reserved >= held.max {
		return false
	}

	remaining := held.max - held.reserved
	chunkSize := initialPreReleaseBufferSize
	if len(held.chunks) > 0 {
		previousSize := len(held.chunks[len(held.chunks)-1])
		if previousSize > 0 && previousSize <= remaining/2 {
			chunkSize = previousSize * 2
		} else {
			chunkSize = remaining
		}
	}
	if chunkSize > remaining {
		chunkSize = remaining
	}
	if chunkSize <= 0 {
		return false
	}

	held.chunks = append(held.chunks, make([]byte, chunkSize))
	held.used = append(held.used, 0)
	held.reserved += chunkSize
	return true
}

func (held *heldBuffer) nextReadBuffer() []byte {
	if held == nil {
		return nil
	}
	for {
		if len(held.chunks) == 0 {
			if !held.grow() {
				return nil
			}
		}
		last := len(held.chunks) - 1
		if held.used[last] < len(held.chunks[last]) {
			return held.chunks[last][held.used[last]:]
		}
		if !held.grow() {
			return nil
		}
	}
}

func (held *heldBuffer) recordRead(n int) error {
	if held == nil {
		if n == 0 {
			return nil
		}
		return fmt.Errorf("cannot record %d bytes in a nil held buffer", n)
	}
	if n < 0 {
		return fmt.Errorf("invalid held read count %d", n)
	}
	if len(held.chunks) == 0 {
		if n == 0 {
			return nil
		}
		return fmt.Errorf("held read count %d exceeds available buffer", n)
	}
	last := len(held.chunks) - 1
	available := len(held.chunks[last]) - held.used[last]
	if n > available {
		return fmt.Errorf("held read count %d exceeds available buffer %d", n, available)
	}
	held.used[last] += n
	return nil
}

func (held *heldBuffer) writeTo(output io.Writer) error {
	if held == nil {
		return nil
	}
	for index, chunk := range held.chunks {
		if err := writeAll(output, chunk[:held.used[index]]); err != nil {
			return err
		}
	}
	return nil
}

type readResult struct {
	n   int
	err error
}

func startRead(input io.Reader, buffer []byte, results chan<- readResult) {
	go func() {
		results <- readInto(input, buffer)
	}()
}

func forwardHeldBufferUntilReleaseWithFailure(output io.Writer, release <-chan struct{}, failures <-chan error, open func() error, held *heldBuffer, readErr error) error {
	select {
	case err, ok := <-failures:
		if !ok {
			return errReleaseFailureChannelClosed
		}
		if err != nil {
			return err
		}
	case <-release:
		if err := commitOpen(open, failures); err != nil {
			return err
		}
	}
	if err := failureReady(failures); err != nil {
		return err
	}
	if err := held.writeTo(output); err != nil {
		return err
	}
	if readErr == io.EOF {
		return nil
	}
	return readErr
}

func forwardDelayedBufferWithFailure(input io.Reader, output io.Writer, release <-chan struct{}, failures <-chan error, open, completeEmpty func() error, held *heldBuffer) error {
	readRequests := make(chan []byte)
	readResults := make(chan readResult, 1)
	go readWorker(input, readRequests, readResults)
	defer close(readRequests)

	for {
		if err := failureReady(failures); err != nil {
			return err
		}
		if releaseReady(release) {
			if err := commitOpen(open, failures); err != nil {
				return err
			}
			return forwardHeldBufferAndCopy(input, output, held)
		}
		readBuffer := held.nextReadBuffer()
		if len(readBuffer) == 0 {
			break
		}
		readRequests <- readBuffer

		var (
			result   readResult
			haveRead bool
		)
		// Prefer a result that is already available so the readiness check
		// below can observe a selected release that became ready while the read
		// completed. This keeps a completed pre-release read from starting
		// another read.
		select {
		case err, ok := <-failures:
			if !ok {
				return errReleaseFailureChannelClosed
			}
			if err != nil {
				return err
			}
		case result = <-readResults:
			haveRead = true
		default:
			select {
			case err, ok := <-failures:
				if !ok {
					return errReleaseFailureChannelClosed
				}
				if err != nil {
					return err
				}
			case <-release:
				if err := commitOpen(open, failures); err != nil {
					return err
				}
				if err := held.writeTo(output); err != nil {
					return err
				}
				return forwardReadResultWithCompletion(input, output, readBuffer, <-readResults, completeEmpty)
			case result = <-readResults:
				haveRead = true
			}
		}
		if !haveRead {
			return errReleaseFailureChannelClosed
		}

		if err := held.recordRead(result.n); err != nil {
			return err
		}
		if result.err != nil {
			return forwardHeldBufferUntilReleaseWithFailure(output, release, failures, open, held, result.err)
		}

		// A release can become ready immediately after the read result.
		// Check again before requesting another bounded-buffer read.
		if releaseReady(release) {
			if err := commitOpen(open, failures); err != nil {
				return err
			}
			return forwardHeldBufferAndCopy(input, output, held)
		}
	}

	if err := waitForRelease(release, failures, open); err != nil {
		return err
	}
	if err := failureReady(failures); err != nil {
		return err
	}
	if err := held.writeTo(output); err != nil {
		return err
	}
	_, err := io.Copy(output, input)
	return err
}

func releaseReady(release <-chan struct{}) bool {
	select {
	case <-release:
		return true
	default:
		return false
	}
}

func failureReady(failures <-chan error) error {
	if failures == nil {
		return nil
	}
	select {
	case err, ok := <-failures:
		if !ok {
			return errReleaseFailureChannelClosed
		}
		return err
	default:
		return nil
	}
}

func commitOpen(open func() error, failures <-chan error) error {
	if err := failureReady(failures); err != nil {
		return err
	}
	if open == nil {
		return nil
	}
	return open()
}

func waitForRelease(release <-chan struct{}, failures <-chan error, open func() error) error {
	if err := failureReady(failures); err != nil {
		return err
	}
	select {
	case err, ok := <-failures:
		if !ok {
			return errReleaseFailureChannelClosed
		}
		if err != nil {
			return err
		}
	case <-release:
		return commitOpen(open, failures)
	}
	return nil
}

func forwardHeldBufferAndCopy(input io.Reader, output io.Writer, held *heldBuffer) error {
	if err := held.writeTo(output); err != nil {
		return err
	}
	_, err := io.Copy(output, input)
	return err
}

func readWorker(input io.Reader, requests <-chan []byte, results chan<- readResult) {
	// The buffered result lets a completed Read report back after an output
	// error, while the request channel is closed by the caller. Generic readers
	// cannot be canceled, so this avoids leaving the worker blocked on send.
	for buffer := range requests {
		results <- readInto(input, buffer)
	}
}

func readInto(input io.Reader, buffer []byte) readResult {
	n, err := input.Read(buffer)
	if n < 0 || n > len(buffer) {
		return readResult{err: fmt.Errorf("invalid input read count %d", n)}
	}
	return readResult{n: n, err: err}
}

func forwardReadResultWithCompletion(input io.Reader, output io.Writer, readBuffer []byte, result readResult, completeEmpty func() error) error {
	if result.n > 0 {
		if err := writeAll(output, readBuffer[:result.n]); err != nil {
			return err
		}
	}
	if result.err != nil {
		if result.err == io.EOF {
			if result.n == 0 {
				return completeEmptyInput(completeEmpty, nil)
			}
			return nil
		}
		return result.err
	}
	_, err := io.Copy(output, input)
	return err
}

func completeEmptyInput(completeEmpty func() error, failures <-chan error) error {
	if err := failureReady(failures); err != nil {
		return err
	}
	if completeEmpty == nil {
		return nil
	}
	return completeEmpty()
}

func writeAll(output io.Writer, data []byte) error {
	for len(data) > 0 {
		n, err := output.Write(data)
		if n < 0 || n > len(data) {
			return fmt.Errorf("invalid output write count %d", n)
		}
		data = data[n:]
		if err != nil {
			return err
		}
		if n == 0 {
			return io.ErrShortWrite
		}
	}
	return nil
}

func writeDiagnostic(diagnostics io.Writer, err error) {
	_, _ = fmt.Fprintln(diagnostics, err)
}
