//go:build aix || darwin || dragonfly || freebsd || illumos || linux || netbsd || openbsd || solaris

package main

// This file verifies the optional runtime event interface without coupling
// consumers to the release coordinator's internal condition latches.

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestEventsFDDoesNotOverrideHelpOrDescribePriority(t *testing.T) {
	var output, diagnostics bytes.Buffer
	if status := run([]string{"--events-fd=9", "--help"}, describePanicReader{}, &output, &diagnostics); status != 0 {
		t.Fatalf("help status = %d, diagnostics = %q", status, diagnostics.String())
	}
	if got, want := output.String(), helpText; got != want {
		t.Fatalf("help output = %q, want help text", got)
	}
	if diagnostics.Len() != 0 {
		t.Fatalf("help diagnostics = %q, want empty", diagnostics.String())
	}

	output.Reset()
	diagnostics.Reset()
	if status := run([]string{"--events-fd=9", "--describe"}, describePanicReader{}, &output, &diagnostics); status == 0 {
		t.Fatal("--describe combination unexpectedly succeeded")
	}
	if output.Len() != 0 {
		t.Fatalf("describe combination output = %q, want empty", output.String())
	}
}

func TestRunEventsAreOrderedJSONLAndPrecedeData(t *testing.T) {
	if !eventFDSupported() {
		t.Skip("event FD transport is unsupported on this target")
	}
	eventsFile := openEventFile(t)
	defer eventsFile.Close()

	var output, diagnostics bytes.Buffer
	args := []string{"--events-fd=" + strconv.FormatUint(uint64(eventsFile.Fd()), 10), "duration:0s"}
	if status := run(args, strings.NewReader("payload"), &output, &diagnostics); status != 0 {
		t.Fatalf("run status = %d, diagnostics = %q", status, diagnostics.String())
	}
	if got, want := output.String(), "payload"; got != want {
		t.Fatalf("output = %q, want %q", got, want)
	}
	if diagnostics.Len() != 0 {
		t.Fatalf("successful event run wrote diagnostics: %q", diagnostics.String())
	}

	contents := readEventFile(t, eventsFile)
	lines := strings.Split(strings.TrimSuffix(contents, "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("event line count = %d, want 2: %q", len(lines), contents)
	}
	var records []map[string]any
	for _, line := range lines {
		var record map[string]any
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			t.Fatalf("decode event %q: %v", line, err)
		}
		records = append(records, record)
		if _, ok := record["event"]; !ok {
			t.Errorf("event %q has no event field", line)
		}
		timestamp, ok := record["timestamp"].(string)
		if !ok {
			t.Errorf("event %q has no string timestamp", line)
			continue
		}
		parsed, err := time.Parse(time.RFC3339Nano, timestamp)
		if err != nil {
			t.Errorf("event timestamp %q is not RFC3339Nano: %v", timestamp, err)
		} else if parsed.Location() != time.UTC {
			t.Errorf("event timestamp %q location = %v, want UTC", timestamp, parsed.Location())
		}
	}
	if got, want := records[0]["event"], "release-selected"; got != want {
		t.Errorf("first event = %v, want %q", got, want)
	}
	if got, want := records[1]["event"], "stream-open"; got != want {
		t.Errorf("second event = %v, want %q", got, want)
	}
}

func TestRunFileAndGroupEmitsOneOpenTransition(t *testing.T) {
	if !eventFDSupported() {
		t.Skip("event FD transport is unsupported on this target")
	}
	eventsFile := openEventFile(t)
	defer eventsFile.Close()
	readyFile, err := os.CreateTemp(t.TempDir(), "dam-ready-")
	if err != nil {
		t.Fatal(err)
	}
	readyPath := readyFile.Name()
	if err := readyFile.Close(); err != nil {
		t.Fatalf("close ready file: %v", err)
	}

	var output, diagnostics bytes.Buffer
	condition := "file:" + readyPath + " && duration:0s"
	args := []string{"--events-fd", strconv.FormatUint(uint64(eventsFile.Fd()), 10), condition}
	if status := run(args, strings.NewReader("payload"), &output, &diagnostics); status != 0 {
		t.Fatalf("run status = %d, diagnostics = %q", status, diagnostics.String())
	}
	contents := readEventFile(t, eventsFile)
	if got := strings.Count(contents, `"event":"release-selected"`); got != 1 {
		t.Fatalf("release-selected count = %d, want 1: %q", got, contents)
	}
	if got := strings.Count(contents, `"event":"stream-open"`); got != 1 {
		t.Fatalf("stream-open count = %d, want 1: %q", got, contents)
	}
}

func TestRunEventsSuppressEmptyInput(t *testing.T) {
	eventsFile := openEventFile(t)
	defer eventsFile.Close()

	var output, diagnostics bytes.Buffer
	args := []string{"--events-fd", strconv.FormatUint(uint64(eventsFile.Fd()), 10), "duration:1s"}
	if status := run(args, strings.NewReader(""), &output, &diagnostics); status != 0 {
		t.Fatalf("run status = %d, diagnostics = %q", status, diagnostics.String())
	}
	if got := readEventFile(t, eventsFile); got != "" {
		t.Fatalf("empty input emitted events %q", got)
	}
}

func TestRunStartupReleaseReportsBothEventsBeforeInput(t *testing.T) {
	if !eventFDSupported() {
		t.Skip("event FD transport is unsupported on this target")
	}
	eventsFile := openEventFile(t)
	defer eventsFile.Close()
	started := make(chan struct{})
	unblock := make(chan struct{})
	input := blockingEOFReader{started: started, unblock: unblock}
	var output, diagnostics bytes.Buffer
	args := []string{"--events-fd=" + strconv.FormatUint(uint64(eventsFile.Fd()), 10), "duration:0s"}
	done := make(chan int, 1)
	go func() {
		done <- run(args, input, &output, &diagnostics)
	}()
	select {
	case <-started:
	case <-time.After(testTimeout):
		t.Fatal("run did not start its input read")
	}
	contents := readEventFile(t, eventsFile)
	if got, want := strings.Count(string(contents), `"event":"release-selected"`), 1; got != want {
		t.Fatalf("release-selected count before input completion = %d, want %d: %q", got, want, contents)
	}
	if got, want := strings.Count(string(contents), `"event":"stream-open"`), 1; got != want {
		t.Fatalf("stream-open count before input completion = %d, want %d: %q", got, want, contents)
	}
	close(unblock)
	select {
	case status := <-done:
		if status != 0 {
			t.Fatalf("run status = %d, diagnostics = %q", status, diagnostics.String())
		}
	case <-time.After(testTimeout):
		t.Fatal("run did not finish after unblocking input")
	}
}

func TestRunRejectsBrokenEventFDBeforeInput(t *testing.T) {
	var output, diagnostics bytes.Buffer
	if status := run([]string{"--events-fd=999999", "duration:0s"}, describePanicReader{}, &output, &diagnostics); status != 2 {
		t.Fatalf("run status = %d, want 2; diagnostics = %q", status, diagnostics.String())
	}
	if output.Len() != 0 {
		t.Fatalf("output = %q, want empty", output.String())
	}
	if !strings.Contains(diagnostics.String(), "invalid --events-fd") {
		t.Fatalf("diagnostics = %q, want invalid event fd", diagnostics.String())
	}
}

type eventTestPipe struct {
	readEnd, writeEnd *os.File
	readFD, writeFD   int
}

func openEventFile(t *testing.T) *eventTestPipe {
	t.Helper()
	readEnd, file, err := os.Pipe()
	if err != nil {
		t.Fatalf("create event pipe: %v", err)
	}
	fd := file.Fd()
	if fd < 3 {
		file.Close()
		readEnd.Close()
		t.Skipf("event file received reserved fd %d", fd)
	}
	if err := syscall.SetNonblock(int(fd), true); err != nil {
		file.Close()
		readEnd.Close()
		t.Fatalf("make event pipe nonblocking: %v", err)
	}
	readFD := int(readEnd.Fd())
	if err := syscall.SetNonblock(readFD, true); err != nil {
		file.Close()
		readEnd.Close()
		t.Fatalf("make event pipe reader nonblocking: %v", err)
	}
	return &eventTestPipe{readEnd: readEnd, writeEnd: file, readFD: readFD, writeFD: int(fd)}
}

func (pipe *eventTestPipe) Fd() uintptr { return uintptr(pipe.writeFD) }

func (pipe *eventTestPipe) Close() {
	_ = pipe.writeEnd.Close()
	_ = pipe.readEnd.Close()
}

func readEventFile(t *testing.T, pipe *eventTestPipe) string {
	t.Helper()
	var contents []byte
	buffer := make([]byte, 4096)
	for {
		n, err := syscall.Read(pipe.readFD, buffer)
		if n > 0 {
			contents = append(contents, buffer[:n]...)
		}
		if err == syscall.EAGAIN || err == syscall.EWOULDBLOCK || n == 0 {
			break
		}
		if err != nil {
			t.Fatalf("read event pipe: %v", err)
		}
	}
	return string(contents)
}

type blockingEOFReader struct {
	started chan<- struct{}
	unblock <-chan struct{}
}

func (reader blockingEOFReader) Read([]byte) (int, error) {
	close(reader.started)
	<-reader.unblock
	return 0, io.EOF
}
