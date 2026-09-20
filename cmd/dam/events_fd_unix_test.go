//go:build aix || darwin || dragonfly || freebsd || illumos || linux || netbsd || openbsd || solaris

package main

// This file locks the Unix descriptor ownership and flag restoration contract.

import (
	"bytes"
	"errors"
	"io"
	"os"
	"strconv"
	"strings"
	"syscall"
	"testing"
)

func TestRunEventFDRestoresOriginalUnixFlags(t *testing.T) {
	eventsFile := openEventFile(t)
	defer eventsFile.Close()
	before, err := eventFDFlags(int(eventsFile.Fd()))
	if err != nil {
		t.Fatalf("read original event fd flags: %v", err)
	}
	var output, diagnostics bytes.Buffer
	args := []string{"--events-fd", strconv.FormatUint(uint64(eventsFile.Fd()), 10), "duration:0s"}
	status, cleanup := execute(args, strings.NewReader("payload"), &output, &diagnostics)
	if status != 0 {
		t.Fatalf("run status = %d, diagnostics = %q", status, diagnostics.String())
	}
	after, err := eventFDFlags(int(eventsFile.Fd()))
	if err != nil {
		t.Fatalf("read restored event fd flags: %v", err)
	}
	if after != before {
		t.Fatalf("event fd flags after run = %#x, want original %#x", after, before)
	}
	if cleanup != nil {
		cleanup()
	}
}

func TestRunFullEventPipeDisablesObservationAndContinuesData(t *testing.T) {
	readEnd, writeEnd, err := os.Pipe()
	if err != nil {
		t.Fatalf("create event pipe: %v", err)
	}
	defer readEnd.Close()
	defer writeEnd.Close()

	writeFD := int(writeEnd.Fd())
	originalFlags, err := eventFDFlags(writeFD)
	if err != nil {
		t.Fatalf("read event pipe flags: %v", err)
	}
	if err := syscall.SetNonblock(writeFD, true); err != nil {
		t.Fatalf("make event pipe nonblocking for setup: %v", err)
	}
	fill := bytes.Repeat([]byte{'x'}, 32*1024)
	for {
		_, writeErr := syscall.Write(writeFD, fill)
		if writeErr == nil {
			continue
		}
		if !errors.Is(writeErr, syscall.EAGAIN) && !errors.Is(writeErr, syscall.EWOULDBLOCK) {
			t.Fatalf("fill event pipe: %v", writeErr)
		}
		break
	}
	if err := setEventFDFlags(writeFD, originalFlags); err != nil {
		t.Fatalf("restore event pipe flags before run: %v", err)
	}

	var output, diagnostics bytes.Buffer
	args := []string{"--events-fd=" + strconv.FormatUint(uint64(writeEnd.Fd()), 10), "duration:0s"}
	if status := run(args, strings.NewReader("payload"), &output, &diagnostics); status != 0 {
		t.Fatalf("run status = %d, diagnostics = %q", status, diagnostics.String())
	}
	if got, want := output.String(), "payload"; got != want {
		t.Fatalf("output = %q, want %q", got, want)
	}
	if got := strings.Count(diagnostics.String(), "events disabled:"); got != 1 {
		t.Fatalf("events-disabled warning count = %d, want 1: %q", got, diagnostics.String())
	}
	afterFlags, err := eventFDFlags(writeFD)
	if err != nil {
		t.Fatalf("read event pipe flags after failed event write: %v", err)
	}
	if afterFlags != originalFlags {
		t.Fatalf("event pipe flags after failed event write = %#x, want %#x", afterFlags, originalFlags)
	}
	_ = writeEnd.Close()
	if _, err := io.Copy(io.Discard, readEnd); err != nil {
		t.Fatalf("drain filled event pipe: %v", err)
	}
}
