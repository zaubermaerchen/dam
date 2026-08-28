package main

// This file implements dam's argument parsing, version reporting, release
// coordination, and delayed stdin-to-stdout forwarding.

import (
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

const preReleaseBufferSize = 64 * 1024

// Keep the initial allocation small so large configured limits are only paid
// for when input actually fills the pre-release buffer.
const initialPreReleaseBufferSize = 4 * 1024

const helpText = `Usage:
  dam DEADLINE [--buffer-size SIZE]
  dam [DEADLINE] --release-on TYPE:SOURCE [--release-on TYPE:SOURCE]... [--buffer-size SIZE]
  dam --help
  dam --version

Hold pipeline output until a release condition is met.

Arguments:
  DEADLINE
        A relative Go duration (such as 500ms, 3s, or 2m) starts after the
        first non-empty stdin read. An absolute local datetime in
        YYYY-MM-DDTHH:MM[:SS] form is monitored from startup in local time.

Options:
  --release-on TYPE:SOURCE
        Release when an external condition is met. May be repeated.

        Supported conditions:
          signal:USR1, signal:SIGUSR1
              Release on SIGUSR1 (supported Unix platforms only).
          signal:USR2, signal:SIGUSR2
              Release on SIGUSR2 (supported Unix platforms only).
          file:PATH
              Release when PATH exists as a regular file (supported on all platforms).

  --buffer-size SIZE
        Set the maximum pre-release buffer size (default: 64K).
        SIZE is a positive byte count or a binary K/k, M/m, or G/g value.

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
	return runWithClock(args, input, output, diagnostics, defaultRuntimeClock())
}

func runWithClock(args []string, input io.Reader, output, diagnostics io.Writer, clock runtimeClock) int {
	status, cleanup := executeWithClock(args, input, output, diagnostics, nil, clock)
	if cleanup != nil {
		cleanup()
	}
	return status
}

func execute(args []string, input io.Reader, output, diagnostics io.Writer) (int, func()) {
	return executeWithReady(args, input, output, diagnostics, nil)
}

func executeWithReady(args []string, input io.Reader, output, diagnostics io.Writer, ready func()) (int, func()) {
	return executeWithClock(args, input, output, diagnostics, ready, defaultRuntimeClock())
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

	clock = clock.normalized()
	config, err := parseConfigAt(args, clock.location)
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
	stopDeadline := startDeadlineMonitor(config.deadline, clock.now, coordinator, clock.newTimer)
	cleanup := func() {
		if stopDeadline != nil {
			stopDeadline()
		}
		monitor.Close()
	}
	if ready != nil {
		ready()
	}

	if err := forwardWithFailureAndBuffer(input, output, config.delay, monitor.Release(), monitor.Failures(), coordinator.requestOpen, coordinator.completeEmpty, config.bufferSize); err != nil {
		writeDiagnostic(diagnostics, err)
		return 1, cleanup
	}
	return 0, cleanup
}

type runConfig struct {
	delay    *time.Duration
	deadline *time.Time
	signals  []string
	files    []string

	bufferSize int
}

func parseConfig(args []string) (runConfig, error) {
	return parseConfigAt(args, time.Local)
}

func parseConfigAt(args []string, location *time.Location) (runConfig, error) {
	location = normalizeLocation(location)
	config := runConfig{bufferSize: preReleaseBufferSize}
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
		case arg == "--buffer-size":
			index++
			if index == len(args) {
				return runConfig{}, fmt.Errorf("missing value for --buffer-size")
			}
			bufferSize, err := parseBufferSize(args[index])
			if err != nil {
				return runConfig{}, err
			}
			config.bufferSize = bufferSize
		case strings.HasPrefix(arg, "--buffer-size="):
			bufferSize, err := parseBufferSize(strings.TrimPrefix(arg, "--buffer-size="))
			if err != nil {
				return runConfig{}, err
			}
			config.bufferSize = bufferSize
		case strings.HasPrefix(arg, "--"):
			return runConfig{}, fmt.Errorf("unknown option %q", arg)
		default:
			if config.delay != nil || config.deadline != nil {
				if isDeadlineToken(arg, location) {
					// Keep the established diagnostic for compatibility even
					// though the positional value now accepts an absolute deadline.
					return runConfig{}, fmt.Errorf("multiple durations are not allowed")
				}
				return runConfig{}, fmt.Errorf("unexpected argument %q", arg)
			}
			delay, deadline, err := parseDeadlineArgument(arg, location)
			if err != nil {
				return runConfig{}, err
			}
			config.delay = delay
			config.deadline = deadline
		}
	}
	if config.delay == nil && config.deadline == nil && len(config.signals) == 0 && len(config.files) == 0 {
		return runConfig{}, fmt.Errorf("usage: dam [DEADLINE] [--release-on TYPE:SOURCE]")
	}
	return config, nil
}

func parseDeadlineArgument(value string, location *time.Location) (*time.Duration, *time.Time, error) {
	delay, durationErr := time.ParseDuration(value)
	if durationErr == nil {
		if delay < 0 {
			return nil, nil, fmt.Errorf("duration must not be negative")
		}
		return &delay, nil, nil
	}

	deadline, absoluteErr := parseAbsoluteDeadline(value, location)
	if absoluteErr == nil {
		return nil, &deadline, nil
	}
	return nil, nil, fmt.Errorf("invalid deadline %q: want a Go duration or YYYY-MM-DDTHH:MM[:SS] local datetime", value)
}

func isDeadlineToken(value string, location *time.Location) bool {
	if _, err := time.ParseDuration(value); err == nil {
		return true
	}
	_, err := parseAbsoluteDeadline(value, location)
	return err == nil
}

func normalizeLocation(location *time.Location) *time.Location {
	if location != nil {
		return location
	}
	if time.Local != nil {
		return time.Local
	}
	return time.UTC
}

func parseAbsoluteDeadline(value string, location *time.Location) (time.Time, error) {
	location = normalizeLocation(location)
	if len(value) != len("2006-01-02T15:04") && len(value) != len("2006-01-02T15:04:05") {
		return time.Time{}, fmt.Errorf("absolute deadline must use YYYY-MM-DDTHH:MM[:SS]")
	}
	if value[4] != '-' || value[7] != '-' || value[10] != 'T' || value[13] != ':' {
		return time.Time{}, fmt.Errorf("absolute deadline must use YYYY-MM-DDTHH:MM[:SS]")
	}
	if len(value) == 19 && value[16] != ':' {
		return time.Time{}, fmt.Errorf("absolute deadline must use YYYY-MM-DDTHH:MM:SS")
	}
	if !allASCIIDigits(value[:4]) || !allASCIIDigits(value[5:7]) || !allASCIIDigits(value[8:10]) || !allASCIIDigits(value[11:13]) || !allASCIIDigits(value[14:16]) {
		return time.Time{}, fmt.Errorf("absolute deadline contains non-numeric fields")
	}
	year := parseASCIIDigits(value[:4])
	month := time.Month(parseASCIIDigits(value[5:7]))
	day := parseASCIIDigits(value[8:10])
	hour := parseASCIIDigits(value[11:13])
	minute := parseASCIIDigits(value[14:16])
	second := 0
	if len(value) == 19 {
		if !allASCIIDigits(value[17:19]) {
			return time.Time{}, fmt.Errorf("absolute deadline contains non-numeric fields")
		}
		second = parseASCIIDigits(value[17:19])
	}
	if year < 1 || year > 9999 {
		return time.Time{}, fmt.Errorf("absolute deadline year must be between 0001 and 9999")
	}

	parsed := time.Date(year, month, day, hour, minute, second, 0, location)
	if !sameLocalDateTime(parsed, year, month, day, hour, minute, second) {
		return time.Time{}, fmt.Errorf("absolute deadline is not a valid local datetime")
	}
	return earliestLocalInstant(parsed, year, month, day, hour, minute, second, location), nil
}

func allASCIIDigits(value string) bool {
	if value == "" {
		return false
	}
	for index := 0; index < len(value); index++ {
		if value[index] < '0' || value[index] > '9' {
			return false
		}
	}
	return true
}

func parseASCIIDigits(value string) int {
	result := 0
	for index := 0; index < len(value); index++ {
		result = result*10 + int(value[index]-'0')
	}
	return result
}

func sameLocalDateTime(value time.Time, year int, month time.Month, day, hour, minute, second int) bool {
	return value.Year() == year && value.Month() == month && value.Day() == day &&
		value.Hour() == hour && value.Minute() == minute && value.Second() == second && value.Nanosecond() == 0
}

// earliestLocalInstant also considers the other side of a fall-back
// transition. time.Date deliberately leaves which duplicate instant it
// chooses unspecified, so selecting by instant here keeps the CLI behavior
// stable across Go versions and time zones.
func earliestLocalInstant(parsed time.Time, year int, month time.Month, day, hour, minute, second int, location *time.Location) time.Time {
	wall := time.Date(year, month, day, hour, minute, second, 0, time.UTC)
	offsets := make(map[int]struct{})
	for _, delta := range []time.Duration{
		-72 * time.Hour, -48 * time.Hour, -36 * time.Hour, -24 * time.Hour,
		-12 * time.Hour, -6 * time.Hour, -3 * time.Hour, -2 * time.Hour,
		-time.Hour, -30 * time.Minute, 0, 30 * time.Minute, time.Hour,
		2 * time.Hour, 3 * time.Hour, 6 * time.Hour, 12 * time.Hour,
		24 * time.Hour, 36 * time.Hour, 48 * time.Hour, 72 * time.Hour,
	} {
		_, offset := parsed.Add(delta).Zone()
		offsets[offset] = struct{}{}
	}

	best := parsed
	for offset := range offsets {
		candidate := wall.Add(-time.Duration(offset) * time.Second).In(location)
		if sameLocalDateTime(candidate, year, month, day, hour, minute, second) && candidate.Before(best) {
			best = candidate
		}
	}
	return best
}

type deadlineMonitorState struct {
	done chan struct{}

	stopOnce  sync.Once
	mu        sync.Mutex
	stopped   bool
	stopTimer func()
}

func (state *deadlineMonitorState) arm(stopTimer func()) bool {
	state.mu.Lock()
	if state.stopped {
		state.mu.Unlock()
		if stopTimer != nil {
			stopTimer()
		}
		return false
	}
	state.stopTimer = stopTimer
	state.mu.Unlock()
	return true
}

func (state *deadlineMonitorState) disarm() {
	state.mu.Lock()
	stopTimer := state.stopTimer
	state.stopTimer = nil
	state.mu.Unlock()
	if stopTimer != nil {
		stopTimer()
	}
}

func (state *deadlineMonitorState) stop() {
	state.stopOnce.Do(func() {
		state.mu.Lock()
		state.stopped = true
		stopTimer := state.stopTimer
		state.stopTimer = nil
		state.mu.Unlock()
		close(state.done)
		if stopTimer != nil {
			stopTimer()
		}
	})
}

func releaseChannelReady(release <-chan struct{}) bool {
	if release == nil {
		return false
	}
	select {
	case <-release:
		return true
	default:
		return false
	}
}

func startDeadlineMonitor(deadline *time.Time, now func() time.Time, coordinator *releaseCoordinator, newTimer func(time.Duration) (<-chan time.Time, func())) func() {
	if deadline == nil || coordinator == nil || releaseChannelReady(coordinator.release) {
		return nil
	}
	if now == nil {
		now = time.Now
	}
	current := now()
	wait := deadline.Sub(current)
	if wait <= 0 {
		// An already elapsed deadline is an immediately satisfied release
		// condition. Initial file probes have completed before this function is
		// called, so their fatal results still retain precedence.
		_ = coordinator.requestOpen()
		return nil
	}
	if newTimer == nil {
		newTimer = defaultRuntimeClock().newTimer
	}

	// Arm the first timer synchronously. This ensures the absolute deadline
	// is active before executeWithClock invokes readiness hooks or starts the
	// first stdin read.
	state := &deadlineMonitorState{done: make(chan struct{})}
	current = now()
	wait = deadline.Sub(current)
	if wait <= 0 {
		_ = coordinator.requestOpen()
		return nil
	}
	capped := false
	if !current.Add(wait).Equal(*deadline) {
		wait = time.Duration(1<<63 - 1)
		capped = true
	}
	timerC, stopTimer := newTimer(wait)
	if timerC == nil {
		if stopTimer != nil {
			stopTimer()
		}
		return nil
	}
	if !state.arm(stopTimer) {
		return nil
	}

	go func() {
		for {
			select {
			case <-timerC:
				state.disarm()
				if !capped {
					_ = coordinator.requestOpen()
					return
				}
				if releaseChannelReady(coordinator.release) {
					return
				}
				current := now()
				wait := deadline.Sub(current)
				if wait <= 0 {
					_ = coordinator.requestOpen()
					return
				}
				capped = false
				if !current.Add(wait).Equal(*deadline) {
					wait = time.Duration(1<<63 - 1)
					capped = true
				}
				timerC, stopTimer = newTimer(wait)
				if timerC == nil {
					if stopTimer != nil {
						stopTimer()
					}
					return
				}
				if !state.arm(stopTimer) {
					return
				}
			case <-coordinator.release:
				state.disarm()
				return
			case <-state.done:
				state.disarm()
				return
			}
		}
	}()
	return state.stop
}

func parseBufferSize(value string) (int, error) {
	if value == "" {
		return 0, fmt.Errorf("invalid buffer size %q", value)
	}

	multiplier := uint64(1)
	digits := value
	switch value[len(value)-1] {
	case 'K', 'k':
		multiplier = 1 << 10
		digits = value[:len(value)-1]
	case 'M', 'm':
		multiplier = 1 << 20
		digits = value[:len(value)-1]
	case 'G', 'g':
		multiplier = 1 << 30
		digits = value[:len(value)-1]
	}
	if digits == "" {
		return 0, fmt.Errorf("invalid buffer size %q", value)
	}
	for index := 0; index < len(digits); index++ {
		if digits[index] < '0' || digits[index] > '9' {
			return 0, fmt.Errorf("invalid buffer size %q", value)
		}
	}

	bytes, err := strconv.ParseUint(digits, 10, 64)
	if err != nil || bytes == 0 {
		return 0, fmt.Errorf("invalid buffer size %q", value)
	}
	maxInt := uint64(^uint(0) >> 1)
	if bytes > maxInt/multiplier {
		return 0, fmt.Errorf("invalid buffer size %q", value)
	}
	return int(bytes * multiplier), nil
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
	return forwardWithFailureAndBuffer(input, output, delay, release, failures, open, completeEmpty, preReleaseBufferSize)
}

func forwardWithFailureAndBuffer(input io.Reader, output io.Writer, delay *time.Duration, release <-chan struct{}, failures <-chan error, open, completeEmpty func() error, bufferSize int) error {
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
			if err := held.recordRead(result.n); err != nil {
				return err
			}
			if result.err != nil {
				return forwardHeldBufferUntilReleaseWithFailure(output, timerC, release, failures, open, held, result.err)
			}
			return forwardDelayedBufferWithFailure(input, output, timerC, release, failures, open, completeEmpty, held)
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

func (held *heldBuffer) reservedCapacity() int {
	if held == nil {
		return 0
	}
	return held.reserved
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

func heldBufferFromSlice(data []byte, used, max int) *heldBuffer {
	if max < len(data) {
		max = len(data)
	}
	data = data[:len(data):len(data)]
	return &heldBuffer{
		chunks:   [][]byte{data},
		used:     []int{used},
		max:      max,
		reserved: len(data),
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
	return forwardHeldBufferUntilReleaseWithFailure(output, timerC, release, failures, open, heldBufferFromSlice(held, heldN, len(held)), readErr)
}

func forwardHeldBufferUntilReleaseWithFailure(output io.Writer, timerC <-chan time.Time, release <-chan struct{}, failures <-chan error, open func() error, held *heldBuffer, readErr error) error {
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
	if err := held.writeTo(output); err != nil {
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
	return forwardDelayedWithFailureAndBuffer(input, output, timerC, release, failures, open, completeEmpty, held, heldN, len(held))
}

func forwardDelayedWithFailureAndBuffer(input io.Reader, output io.Writer, timerC <-chan time.Time, release <-chan struct{}, failures <-chan error, open, completeEmpty func() error, held []byte, heldN, maxBufferSize int) error {
	return forwardDelayedBufferWithFailure(input, output, timerC, release, failures, open, completeEmpty, heldBufferFromSlice(held, heldN, maxBufferSize))
}

func forwardDelayedBufferWithFailure(input io.Reader, output io.Writer, timerC <-chan time.Time, release <-chan struct{}, failures <-chan error, open, completeEmpty func() error, held *heldBuffer) error {
	readRequests := make(chan []byte)
	readResults := make(chan readResult, 1)
	go readWorker(input, readRequests, readResults)
	defer close(readRequests)

	for {
		if err := failureReady(failures); err != nil {
			return err
		}
		if releaseReady(timerC, release) {
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
				if err := held.writeTo(output); err != nil {
					return err
				}
				return forwardReadResultWithCompletion(input, output, readBuffer, <-readResults, completeEmpty)
			case <-timerC:
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
			return forwardHeldBufferUntilReleaseWithFailure(output, timerC, release, failures, open, held, result.err)
		}

		// A timer/release can become ready immediately after the read result.
		// Check again before requesting another bounded-buffer read.
		if releaseReady(timerC, release) {
			if err := commitOpen(open, failures); err != nil {
				return err
			}
			return forwardHeldBufferAndCopy(input, output, held)
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
	if err := held.writeTo(output); err != nil {
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
	return forwardHeldBufferAndCopy(input, output, heldBufferFromSlice(held, len(held), len(held)))
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
