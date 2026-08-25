package main

// This file implements dam's argument parsing, version reporting, release
// coordination, and delayed stdin-to-stdout forwarding.

import (
	"fmt"
	"io"
	"os"
	"strings"
	"time"
)

const preReleaseBufferSize = 64 * 1024

var version = "dev"

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

	monitor, err := newReleaseMonitor(config.signals)
	if err != nil {
		writeDiagnostic(diagnostics, err)
		return 1, nil
	}
	if ready != nil {
		ready()
	}

	if err := forward(input, output, config.delay, monitor.Release()); err != nil {
		writeDiagnostic(diagnostics, err)
		return 1, monitor.Close
	}
	return 0, monitor.Close
}

type runConfig struct {
	delay   *time.Duration
	signals []string
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
			signal, err := parseReleaseCondition(args[index])
			if err != nil {
				return runConfig{}, err
			}
			config.signals = append(config.signals, signal)
		case strings.HasPrefix(arg, "--release-on="):
			signal, err := parseReleaseCondition(strings.TrimPrefix(arg, "--release-on="))
			if err != nil {
				return runConfig{}, err
			}
			config.signals = append(config.signals, signal)
		case strings.HasPrefix(arg, "--"):
			return runConfig{}, fmt.Errorf("unknown option %q", arg)
		default:
			if config.delay != nil {
				return runConfig{}, fmt.Errorf("multiple durations are not allowed")
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
	if config.delay == nil && len(config.signals) == 0 {
		return runConfig{}, fmt.Errorf("usage: dam [DURATION] [--release-on TYPE:SOURCE]")
	}
	return config, nil
}

func parseReleaseCondition(value string) (string, error) {
	typeName, source, ok := strings.Cut(value, ":")
	if !ok || typeName != "signal" || source != "USR1" && source != "SIGUSR1" {
		return "", fmt.Errorf("invalid release condition %q: want signal:USR1 or signal:SIGUSR1", value)
	}
	return "SIGUSR1", nil
}

func forward(input io.Reader, output io.Writer, delay *time.Duration, release <-chan struct{}) error {
	if delay != nil && *delay == 0 {
		_, err := io.Copy(output, input)
		return err
	}

	held := make([]byte, preReleaseBufferSize)
	firstResults := make(chan readResult, 1)
	startRead(input, held, firstResults)

	for {
		select {
		case <-release:
			// A signal may arrive before the first read has returned. The read
			// still owns the input ordering, so wait for its result before
			// forwarding the now-open stream.
			return forwardReadResult(input, output, held, <-firstResults)
		case result := <-firstResults:
			if result.n == 0 {
				if result.err == io.EOF {
					return nil
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
				return forwardReadResult(input, output, held, result)
			default:
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
				return forwardHeldUntilRelease(output, timerC, release, held, result.n, result.err)
			}
			return forwardDelayed(input, output, timerC, release, held, result.n)
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
	if timerC != nil || release != nil {
		select {
		case <-timerC:
		case <-release:
		}
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
	readRequests := make(chan []byte)
	readResults := make(chan readResult, 1)
	go readWorker(input, readRequests, readResults)
	defer close(readRequests)

	for heldN < len(held) {
		if releaseReady(timerC, release) {
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
		case result = <-readResults:
			haveRead = true
		default:
			select {
			case <-release:
			case <-timerC:
			case result = <-readResults:
				haveRead = true
			}
		}
		if !haveRead {
			if err := writeAll(output, held[:heldN]); err != nil {
				return err
			}
			return forwardReadResult(input, output, readBuffer, <-readResults)
		}

		heldN += result.n
		if result.err != nil {
			return forwardHeldUntilRelease(output, timerC, release, held, heldN, result.err)
		}

		// A timer/release can become ready immediately after the read result.
		// Check again before requesting another bounded-buffer read.
		if releaseReady(timerC, release) {
			return forwardHeldAndCopy(input, output, held[:heldN])
		}
	}

	if timerC != nil || release != nil {
		select {
		case <-timerC:
		case <-release:
		}
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
	if result.n > 0 {
		if err := writeAll(output, readBuffer[:result.n]); err != nil {
			return err
		}
	}
	if result.err != nil {
		if result.err == io.EOF {
			return nil
		}
		return result.err
	}
	_, err := io.Copy(output, input)
	return err
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
