//go:build aix || darwin || dragonfly || freebsd || illumos || linux || netbsd || openbsd || solaris

package main

// This file exercises the production exit boundary with failed event output
// and real normal or saturated stderr pipes.

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestEventWarningExitHelper(t *testing.T) {
	if os.Getenv("DAM_EVENT_WARNING_EXIT_HELPER") == "" {
		return
	}
	os.Args = []string{"dam", "duration:0s", "--events-fd=3"}
	if os.Getenv("DAM_EVENT_WARNING_EXIT_HELPER") == "output-error" {
		_ = os.Stdout.Close()
	}
	main()
}

func TestEventWarningAtProcessExit(t *testing.T) {
	for _, mode := range []string{"normal", "blocked", "output-error"} {
		t.Run(mode, func(t *testing.T) {
			_, eventWrite := fullExitTestPipe(t)
			cmd := exec.Command(os.Args[0], "-test.run=^TestEventWarningExitHelper$")
			cmd.Env = append(os.Environ(), "DAM_EVENT_WARNING_EXIT_HELPER="+mode, "GOMAXPROCS=1")
			cmd.ExtraFiles = []*os.File{eventWrite}
			cmd.Stdin = strings.NewReader("payload")
			var output, diagnostics bytes.Buffer
			cmd.Stdout = &output
			cmd.Stderr = &diagnostics
			if mode == "blocked" {
				_, stderrWrite := fullExitTestPipe(t)
				// Unlike the event FD, stderr deliberately blocks on a full pipe.
				if err := syscall.SetNonblock(int(stderrWrite.Fd()), false); err != nil {
					t.Fatal(err)
				}
				cmd.Stderr = stderrWrite
			}
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			done := make(chan error, 1)
			go func() { done <- cmd.Wait() }()
			var err error
			select {
			case err = <-done:
			case <-time.After(3 * time.Second):
				_ = cmd.Process.Kill()
				<-done
				t.Fatal("process exit blocked on warning")
			}
			if mode == "output-error" {
				var exitErr *exec.ExitError
				if !errors.As(err, &exitErr) || exitErr.ExitCode() != 1 {
					t.Fatalf("exit error = %v, want primary failure status 1", err)
				}
				if !strings.Contains(diagnostics.String(), "file already closed") {
					t.Fatalf("primary diagnostic missing: %q", diagnostics.String())
				}
			} else {
				if err != nil {
					t.Fatalf("exit: %v; stderr: %q", err, diagnostics.String())
				}
				if output.String() != "payload" {
					t.Fatalf("stdout = %q", output.String())
				}
			}
			if mode == "normal" || mode == "output-error" {
				if count := strings.Count(diagnostics.String(), "events disabled:"); count != 1 {
					t.Fatalf("warnings = %d, want 1; stderr = %q", count, diagnostics.String())
				}
			}
		})
	}
}

func fullExitTestPipe(t *testing.T) (*os.File, *os.File) {
	t.Helper()
	var fds [2]int
	if err := syscall.Pipe(fds[:]); err != nil {
		t.Fatal(err)
	}
	reader := os.NewFile(uintptr(fds[0]), "exit-test reader")
	writer := os.NewFile(uintptr(fds[1]), "exit-test writer")
	t.Cleanup(func() { _ = reader.Close() })
	t.Cleanup(func() { _ = writer.Close() })
	fd := fds[1]
	if err := syscall.SetNonblock(fd, true); err != nil {
		t.Fatal(err)
	}
	data := make([]byte, 4096)
	for {
		_, err := syscall.Write(fd, data)
		if errors.Is(err, syscall.EAGAIN) || errors.Is(err, syscall.EWOULDBLOCK) {
			return reader, writer
		}
		if err != nil && !errors.Is(err, syscall.EINTR) {
			t.Fatal(err)
		}
	}
}
