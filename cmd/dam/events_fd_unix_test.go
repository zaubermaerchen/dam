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
	var output, diagnostics bytes.Buffer
	// Reuse the raw descriptor captured before SetNonblock: os.File.Fd resets
	// Go's internally tracked pipe mode to blocking when called later.
	args := []string{"--events-fd=" + strconv.Itoa(writeFD), "duration:0s"}
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
	// A failed observation write may still leave Darwin's kernel-owned write
	// marker set; the caller's pre-existing nonblocking mode must be unchanged.
	wantFlags := originalFlags | syscall.O_NONBLOCK
	if afterFlags&syscall.O_NONBLOCK != wantFlags&syscall.O_NONBLOCK {
		t.Fatalf("event pipe blocking mode after failed event write = %#x, want %#x (full flags after %#x, before %#x)", afterFlags&syscall.O_NONBLOCK, wantFlags&syscall.O_NONBLOCK, afterFlags, originalFlags)
	}
	_ = writeEnd.Close()
	if _, err := io.Copy(io.Discard, readEnd); err != nil {
		t.Fatalf("drain filled event pipe: %v", err)
	}
}

func eventFDFlags(fd int) (int, error) {
	result, _, errno := syscall.Syscall(syscall.SYS_FCNTL, uintptr(fd), uintptr(syscall.F_GETFL), 0)
	if errno != 0 {
		return 0, errno
	}
	return int(result), nil
}
