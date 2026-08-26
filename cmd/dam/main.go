package main

// This file implements dam's argument parsing, version reporting, release
// coordination, and delayed stdin-to-stdout forwarding.

import (
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"
	"time"
)

const preReleaseBufferSize = 64 * 1024

const helpText = `Usage:
  dam DURATION
  dam [DURATION] --release-on TYPE:SOURCE [--release-on TYPE:SOURCE]...
  dam --help
  dam --version

Hold pipeline output until a release condition is met.

Arguments:
  DURATION
        Start the release timer after stdin's first non-empty read completes.
        Uses Go duration syntax, such as 500ms, 3s, or 2m.

Options:
  --release-on TYPE:SOURCE
        Release when an external condition is met. May be repeated.

        Supported conditions:
          signal:USR1, signal:SIGUSR1
              Release on SIGUSR1 (supported Unix platforms only).
          signal:USR2, signal:SIGUSR2
              Release on SIGUSR2 (supported Unix platforms only).
          file:PATH
              Release when PATH exists as a regular file.

  -h, --help
        Show this help and exit.

  --version
        Show version and exit.
`

var version = "dev"

var errReleaseFailureChannelClosed = errors.New("internal error: release failure channel closed")

func main() {
	// Keep the signal monitor registered until os.Exit so a configured SIGUSR1
	// cannot be restored to its terminating default action during normal CLI
	// shutdown. The run wrapper cleans it up for long-lived unit tests.
	status, _ := execute(os.Args[1:], os.Stdin, os.Stdout, os.Stderr)
	os.Exit(status)
}

func run(args []string, input io.Reader, output, diagnostics io.Writer) int {
	status, cleanup := execute(args, input, output, diagnostics)
	if cleanup != nil {
		cleanup()
	}
	return status
}

func execute(args []string, input io.Reader, output, diagnostics io.Writer) (int, func()) {
	return executeWithReady(args, input, output, diagnostics, nil)
}

func executeWithReady(args []string, input io.Reader, output, diagnostics io.Writer, ready func()) (int, func()) {
	if slices.Contains(args, "-h") || slices.Contains(args, "--help") {
		if err := writeAll(output, []byte(helpText)); err != nil {
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

	config, err := parseConfig(args)
	if err != nil {
		writeDiagnostic(diagnostics, err)
		return 1, nil
	}

	coordinator := newReleaseCoordinator(true)
	monitor, err := newReleaseMonitor(config.signals, coordinator)
	if err != nil {
		writeDiagnostic(diagnostics, err)
		return 1, nil
	}
	if len(config.files) > 0 {
		if _, err := newFileMonitor(config.files, coordinator); err != nil {
			writeDiagnostic(diagnostics, err)
			return 1, monitor.Close
		}
	} else if err := coordinator.finishInitial(); err != nil {
		writeDiagnostic(diagnostics, err)
		return 1, monitor.Close
	}
	if err := coordinator.fatalError(); err != nil {
		writeDiagnostic(diagnostics, err)
		return 1, monitor.Close
	}
	if ready != nil {
		ready()
	}

	if err := forwardWithFailure(input, output, config.delay, monitor.Release(), monitor.Failures(), coordinator.requestOpen, coordinator.completeEmpty); err != nil {
		writeDiagnostic(diagnostics, err)
		return 1, monitor.Close
	}
	return 0, monitor.Close
}

type runConfig struct {
	delay   *time.Duration
	signals []string
	files   []string
}

func parseConfig(args []string) (runConfig, error) {
	var config runConfig
	for index := 0; index < len(args); index++ {
		arg := args[index]
		switch {
		case arg == "--release-on":
			index++
			if index == len(args) {
				return runConfig{}, fmt.Errorf("missing value for --release-on")
			}
			condition, err := parseCondition(args[index])
			if err != nil {
				return runConfig{}, err
			}
			config.addCondition(condition)
		case strings.HasPrefix(arg, "--release-on="):
			condition, err := parseCondition(strings.TrimPrefix(arg, "--release-on="))
			if err != nil {
				return runConfig{}, err
			}
			config.addCondition(condition)
		case strings.HasPrefix(arg, "--"):
			return runConfig{}, fmt.Errorf("unknown option %q", arg)
		default:
			if config.delay != nil {
				if _, err := time.ParseDuration(arg); err == nil {
					return runConfig{}, fmt.Errorf("multiple durations are not allowed")
				}
				return runConfig{}, fmt.Errorf("unexpected argument %q", arg)
			}
			delay, err := time.ParseDuration(arg)
			if err != nil {
				return runConfig{}, fmt.Errorf("invalid duration %q: %w", arg, err)
			}
			if delay < 0 {
				return runConfig{}, fmt.Errorf("duration must not be negative")
			}
			config.delay = &delay
		}
	}
	if config.delay == nil && len(config.signals) == 0 && len(config.files) == 0 {
		return runConfig{}, fmt.Errorf("usage: dam [DURATION] [--release-on TYPE:SOURCE]")
	}
	return config, nil
}

type releaseCondition struct {
	kind   string
	source string
}

func (config *runConfig) addCondition(condition releaseCondition) {
	if condition.kind == "signal" {
		config.signals = append(config.signals, condition.source)
		return
	}
	config.files = append(config.files, condition.source)
}

func parseCondition(value string) (releaseCondition, error) {
	typeName, source, ok := strings.Cut(value, ":")
	if !ok {
		return releaseCondition{}, invalidReleaseCondition(value)
	}
	switch typeName {
	case "signal":
		signal, err := parseSignalSource(value, source)
		if err != nil {
			return releaseCondition{}, err
		}
		return releaseCondition{kind: "signal", source: signal}, nil
	case "file":
		if source == "" {
			return releaseCondition{}, fmt.Errorf("invalid release condition %q: file path must not be empty", value)
		}
		return releaseCondition{kind: "file", source: source}, nil
	default:
		return releaseCondition{}, invalidReleaseCondition(value)
	}
}

func parseReleaseCondition(value string) (string, error) {
	condition, err := parseCondition(value)
	if err != nil {
		return "", err
	}
	if condition.kind != "signal" {
		return "", invalidReleaseCondition(value)
	}
	return condition.source, nil
}

func parseSignalSource(value, source string) (string, error) {
	switch source {
	case "USR1", "SIGUSR1":
		return "SIGUSR1", nil
	case "USR2", "SIGUSR2":
		return "SIGUSR2", nil
	default:
		return "", invalidReleaseCondition(value)
	}
}

func invalidReleaseCondition(value string) error {
	return fmt.Errorf("invalid release condition %q: want signal:USR1, signal:SIGUSR1, signal:USR2, signal:SIGUSR2, or file:PATH", value)
}

func forward(input io.Reader, output io.Writer, delay *time.Duration, release <-chan struct{}) error {
	return forwardWithFailure(input, output, delay, release, nil, nil, nil)
}

func forwardWithFailure(input io.Reader, output io.Writer, delay *time.Duration, release <-chan struct{}, failures <-chan error, open, completeEmpty func() error) error {
	if delay != nil && *delay == 0 {
		if err := failureReady(failures); err != nil {
			return err
		}
		if err := commitOpen(open, failures); err != nil {
			return err
		}
		_, err := io.Copy(output, input)
		return err
	}

	held := make([]byte, preReleaseBufferSize)
	firstResults := make(chan readResult, 1)
	startRead(input, held, firstResults)

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
			return forwardReadResultWithCompletion(input, output, held, <-firstResults, completeEmpty)
		case result := <-firstResults:
			if err := failureReady(failures); err != nil {
				return err
			}
			if result.n == 0 {
				if result.err == io.EOF {
					return completeEmptyInput(completeEmpty, failures)
				}
				if result.err != nil {
					return result.err
				}
				startRead(input, held, firstResults)
				continue
			}

			// Prefer a release that was already delivered over starting a new
			// duration window at the same boundary.
			select {
			case <-release:
				if err := commitOpen(open, failures); err != nil {
					return err
				}
				return forwardReadResultWithCompletion(input, output, held, result, completeEmpty)
			default:
			}
			if err := failureReady(failures); err != nil {
				return err
			}

			var timer *time.Timer
			var timerC <-chan time.Time
			if delay != nil {
				// The first non-empty read, rather than process startup, starts
				// the duration window.
				timer = time.NewTimer(*delay)
				timerC = timer.C
			}
			if timer != nil {
				defer timer.Stop()
			}
			if result.err != nil {
				return forwardHeldUntilReleaseWithFailure(output, timerC, release, failures, open, held, result.n, result.err)
			}
			return forwardDelayedWithFailure(input, output, timerC, release, failures, open, completeEmpty, held, result.n)
		}
	}
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

func forwardHeldUntilRelease(output io.Writer, timerC <-chan time.Time, release <-chan struct{}, held []byte, heldN int, readErr error) error {
	return forwardHeldUntilReleaseWithFailure(output, timerC, release, nil, nil, held, heldN, readErr)
}

func forwardHeldUntilReleaseWithFailure(output io.Writer, timerC <-chan time.Time, release <-chan struct{}, failures <-chan error, open func() error, held []byte, heldN int, readErr error) error {
	if timerC != nil || release != nil || failures != nil {
		select {
		case err, ok := <-failures:
			if !ok {
				return errReleaseFailureChannelClosed
			}
			if err != nil {
				return err
			}
		case <-timerC:
			if err := commitOpen(open, failures); err != nil {
				return err
			}
		case <-release:
			if err := commitOpen(open, failures); err != nil {
				return err
			}
		}
	}
	if err := failureReady(failures); err != nil {
		return err
	}
	if err := writeAll(output, held[:heldN]); err != nil {
		return err
	}
	if readErr == io.EOF {
		return nil
	}
	return readErr
}

func forwardDelayed(input io.Reader, output io.Writer, timerC <-chan time.Time, release <-chan struct{}, held []byte, heldN int) error {
	return forwardDelayedWithFailure(input, output, timerC, release, nil, nil, nil, held, heldN)
}

func forwardDelayedWithFailure(input io.Reader, output io.Writer, timerC <-chan time.Time, release <-chan struct{}, failures <-chan error, open, completeEmpty func() error, held []byte, heldN int) error {
	readRequests := make(chan []byte)
	readResults := make(chan readResult, 1)
	go readWorker(input, readRequests, readResults)
	defer close(readRequests)

	for heldN < len(held) {
		if err := failureReady(failures); err != nil {
			return err
		}
		if releaseReady(timerC, release) {
			if err := commitOpen(open, failures); err != nil {
				return err
			}
			return forwardHeldAndCopy(input, output, held[:heldN])
		}
		readBuffer := held[heldN:]
		readRequests <- readBuffer

		var (
			result   readResult
			haveRead bool
		)
		// Prefer a result that is already available so the readiness check
		// below can observe a timer/release that became ready while the read
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
				if err := writeAll(output, held[:heldN]); err != nil {
					return err
				}
				return forwardReadResultWithCompletion(input, output, readBuffer, <-readResults, completeEmpty)
			case <-timerC:
				if err := commitOpen(open, failures); err != nil {
					return err
				}
				if err := writeAll(output, held[:heldN]); err != nil {
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

		heldN += result.n
		if result.err != nil {
			return forwardHeldUntilReleaseWithFailure(output, timerC, release, failures, open, held, heldN, result.err)
		}

		// A timer/release can become ready immediately after the read result.
		// Check again before requesting another bounded-buffer read.
		if releaseReady(timerC, release) {
			if err := commitOpen(open, failures); err != nil {
				return err
			}
			return forwardHeldAndCopy(input, output, held[:heldN])
		}
	}

	if timerC != nil || release != nil {
		if err := waitForRelease(timerC, release, failures, open); err != nil {
			return err
		}
	}
	if err := failureReady(failures); err != nil {
		return err
	}
	if err := writeAll(output, held); err != nil {
		return err
	}
	_, err := io.Copy(output, input)
	return err
}

func releaseReady(timerC <-chan time.Time, release <-chan struct{}) bool {
	select {
	case <-timerC:
		return true
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

func waitForRelease(timerC <-chan time.Time, release <-chan struct{}, failures <-chan error, open func() error) error {
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
	case <-timerC:
		return commitOpen(open, failures)
	case <-release:
		return commitOpen(open, failures)
	}
	return nil
}

func forwardHeldAndCopy(input io.Reader, output io.Writer, held []byte) error {
	if err := writeAll(output, held); err != nil {
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

func forwardReadResult(input io.Reader, output io.Writer, readBuffer []byte, result readResult) error {
	return forwardReadResultWithCompletion(input, output, readBuffer, result, nil)
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
