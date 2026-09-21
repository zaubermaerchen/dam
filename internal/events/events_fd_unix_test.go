//go:build aix || darwin || dragonfly || freebsd || illumos || linux || netbsd || openbsd || solaris

package events

// This file locks Unix descriptor ownership and flag restoration at the
// transport boundary.

import (
	"os"
	"syscall"
	"testing"
)

func TestUnixEventFDWriteRestoresOriginalFlags(t *testing.T) {
	for _, test := range []struct {
		name        string
		nonblocking bool
	}{
		{name: "blocking"},
		{name: "nonblocking", nonblocking: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			file, err := os.CreateTemp(t.TempDir(), "dam-events-")
			if err != nil {
				t.Fatalf("create event file: %v", err)
			}
			defer file.Close()
			callerFD := int(file.Fd())
			if test.nonblocking {
				if err := syscall.SetNonblock(callerFD, true); err != nil {
					t.Fatalf("make event fd nonblocking: %v", err)
				}
			}
			before, err := eventFDFlags(callerFD)
			if err != nil {
				t.Fatalf("read original event fd flags: %v", err)
			}
			writer, err := openEventFD(callerFD)
			if err != nil {
				t.Fatalf("open event fd: %v", err)
			}
			if _, err := writer.Write([]byte("event")); err != nil {
				t.Fatalf("write event: %v", err)
			}
			if err := writer.Close(); err != nil {
				t.Fatalf("close event fd: %v", err)
			}
			after, err := eventFDFlags(callerFD)
			if err != nil {
				t.Fatalf("read restored event fd flags: %v", err)
			}
			if after&syscall.O_NONBLOCK != before&syscall.O_NONBLOCK {
				t.Fatalf("event fd blocking mode after write = %#x, want original %#x", after&syscall.O_NONBLOCK, before&syscall.O_NONBLOCK)
			}
		})
	}
}
