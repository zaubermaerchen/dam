//go:build aix || darwin || dragonfly || freebsd || illumos || linux || netbsd || openbsd || solaris

package main

// This file exercises process-level signal delivery through the new condition
// grammar while another timed/file group is also active.

import (
	"io"
	"os"
	"syscall"
	"testing"
	"time"
)

func TestIssue30SignalSubprocessReleasesMixedTimedFileORGroup(t *testing.T) {
	helper := startSignalHelper(t, "mixed-all")
	defer helper.cleanup()

	select {
	case <-helper.ready:
	case <-time.After(testTimeout):
		t.Fatal("helper did not install its signal monitor")
	}
	if _, err := io.WriteString(helper.stdin, "mixed payload"); err != nil {
		t.Fatalf("write held input: %v", err)
	}
	select {
	case value := <-helper.output:
		t.Fatalf("mixed group released before signal: %q", string([]byte{value}))
	case <-time.After(100 * time.Millisecond):
	}
	if err := helper.signal(syscall.SIGUSR2); err != nil {
		t.Fatalf("send SIGUSR2: %v", err)
	}
	if got := readExactWithTimeout(t, helper.output, len("mixed payload")); got != "mixed payload" {
		t.Fatalf("mixed output = %q, want %q", got, "mixed payload")
	}
	// The signal monitor must remain installed after the alternative group
	// opens the gate; later configured signals must not terminate the process.
	for _, signal := range []os.Signal{syscall.SIGUSR1, syscall.SIGUSR2} {
		if err := helper.signal(signal); err != nil {
			t.Fatalf("send post-release %s: %v", signal, err)
		}
	}
	if _, err := io.WriteString(helper.stdin, "after"); err != nil {
		t.Fatalf("write post-release input: %v", err)
	}
	if got := readExactWithTimeout(t, helper.output, len("after")); got != "after" {
		t.Fatalf("post-release output = %q, want %q", got, "after")
	}
	helper.finish(t)
}

func TestIssue30SignalSubprocessLatchesTimedANDMemberOrder(t *testing.T) {
	helper := startSignalHelper(t, "duration-and-signal")
	defer helper.cleanup()

	select {
	case <-helper.ready:
	case <-time.After(testTimeout):
		t.Fatal("helper did not install its signal monitor")
	}
	if _, err := io.WriteString(helper.stdin, "timed signal payload"); err != nil {
		t.Fatalf("write held input: %v", err)
	}
	select {
	case value := <-helper.output:
		t.Fatalf("timed AND group released before signal: %q", string([]byte{value}))
	default:
	}
	if err := helper.signal(syscall.SIGUSR1); err != nil {
		t.Fatalf("send SIGUSR1: %v", err)
	}
	// The zero duration makes the timed member deterministic; the non-zero
	// timer lifecycle is covered by the injected-clock integration test.
	if got := readExactWithTimeout(t, helper.output, len("timed signal payload")); got != "timed signal payload" {
		t.Fatalf("timed signal output = %q, want %q", got, "timed signal payload")
	}
	helper.finish(t)
}
