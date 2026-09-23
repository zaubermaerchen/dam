//go:build aix || darwin || dragonfly || freebsd || illumos || linux || netbsd || openbsd || solaris

package events

// This file locks Unix descriptor ownership and flag restoration at the
// transport boundary.

import (
	"os"
	"syscall"
	"testing"
)

func TestUnixEventFDRequiresAlreadyNonblockingPipe(t *testing.T) {
	for _, test := range []struct {
		name        string
		nonblocking bool
	}{
		{name: "blocking"},
		{name: "nonblocking", nonblocking: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			readEnd, file, err := os.Pipe()
			if err != nil {
				t.Fatalf("create event pipe: %v", err)
			}
			defer readEnd.Close()
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
			if !test.nonblocking {
				if err == nil {
					writer.Close()
					t.Fatal("blocking event pipe unexpectedly accepted")
				}
				return
			}
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

func TestUnixEventFDRejectsRegularFile(t *testing.T) {
	file, err := os.CreateTemp(t.TempDir(), "dam-events-")
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	if err := syscall.SetNonblock(int(file.Fd()), true); err != nil {
		t.Fatal(err)
	}
	if writer, err := openEventFD(int(file.Fd())); err == nil {
		writer.Close()
		t.Fatal("regular event file unexpectedly accepted")
	}
}

func TestUnixEventFDWriteRejectsChangedBlockingMode(t *testing.T) {
	readEnd, writeEnd, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer readEnd.Close()
	defer writeEnd.Close()
	callerFD := int(writeEnd.Fd())
	if err := syscall.SetNonblock(callerFD, true); err != nil {
		t.Fatal(err)
	}
	writer, err := openEventFD(callerFD)
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()
	if err := syscall.SetNonblock(callerFD, false); err != nil {
		t.Fatal(err)
	}
	if _, err := writer.Write([]byte("event")); err == nil {
		t.Fatal("write accepted pipe changed to blocking mode")
	}
	flags, err := eventFDFlags(callerFD)
	if err != nil {
		t.Fatal(err)
	}
	if flags&syscall.O_NONBLOCK != 0 {
		t.Fatal("event writer changed caller's blocking mode")
	}
}
