//go:build aix || darwin || dragonfly || freebsd || illumos || linux || netbsd || openbsd || solaris

package main

// This file verifies real SIGUSR1 wiring in subprocesses so signal delivery
// does not race with other tests running in the parent test process.

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"os"
	"os/exec"
	"syscall"
	"testing"
	"time"
)

func TestRunSignalSubprocessReleasesBeforeFirstInput(t *testing.T) {
	helper := startSignalHelper(t, "preinput")
	defer helper.cleanup()

	select {
	case <-helper.ready:
	case <-time.After(testTimeout):
		t.Fatal("helper did not install its signal monitor")
	}
	if err := helper.signal(syscall.SIGUSR1); err != nil {
		t.Fatalf("send pre-input SIGUSR1: %v", err)
	}
	if _, err := io.WriteString(helper.stdin, "first"); err != nil {
		t.Fatalf("write first input: %v", err)
	}
	if got := readExactWithTimeout(t, helper.output, len("first")); got != "first" {
		t.Fatalf("first output = %q, want %q", got, "first")
	}
	if _, err := io.WriteString(helper.stdin, "second"); err != nil {
		t.Fatalf("write second input: %v", err)
	}
	if got := readExactWithTimeout(t, helper.output, len("second")); got != "second" {
		t.Fatalf("second output = %q, want %q", got, "second")
	}
	if err := helper.signal(syscall.SIGUSR1); err != nil {
		t.Fatalf("send post-release SIGUSR1: %v", err)
	}
	helper.finish(t)
}

func TestRunSignalSubprocessKeepsTimerWinnerAlive(t *testing.T) {
	helper := startSignalHelper(t, "duration")
	defer helper.cleanup()

	if _, err := io.WriteString(helper.stdin, "first"); err != nil {
		t.Fatalf("write first input: %v", err)
	}
	if got := readExactWithTimeout(t, helper.output, len("first")); got != "first" {
		t.Fatalf("first output = %q, want %q", got, "first")
	}
	if err := helper.signal(syscall.SIGUSR1); err != nil {
		t.Fatalf("send post-timer SIGUSR1: %v", err)
	}
	if _, err := io.WriteString(helper.stdin, "second"); err != nil {
		t.Fatalf("write second input: %v", err)
	}
	if got := readExactWithTimeout(t, helper.output, len("second")); got != "second" {
		t.Fatalf("second output = %q, want %q", got, "second")
	}
	helper.finish(t)
}

func TestRunSignalSubprocessKeepsZeroDelayWinnerAlive(t *testing.T) {
	helper := startSignalHelper(t, "zero")
	defer helper.cleanup()

	if _, err := io.WriteString(helper.stdin, "first"); err != nil {
		t.Fatalf("write first input: %v", err)
	}
	if got := readExactWithTimeout(t, helper.output, len("first")); got != "first" {
		t.Fatalf("first output = %q, want %q", got, "first")
	}
	if err := helper.signal(syscall.SIGUSR1); err != nil {
		t.Fatalf("send post-zero-delay SIGUSR1: %v", err)
	}
	if _, err := io.WriteString(helper.stdin, "second"); err != nil {
		t.Fatalf("write second input: %v", err)
	}
	if got := readExactWithTimeout(t, helper.output, len("second")); got != "second" {
		t.Fatalf("second output = %q, want %q", got, "second")
	}
	helper.finish(t)
}

type signalHelper struct {
	cmd         *exec.Cmd
	stdin       io.WriteCloser
	output      <-chan byte
	diagnostics *bytes.Buffer
	ready       <-chan struct{}
	stderrDone  <-chan struct{}
}

func startSignalHelper(t *testing.T, mode string) *signalHelper {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=TestSignalHelperProcess")
	cmd.Env = append(os.Environ(), "DAM_SIGNAL_HELPER=1", "DAM_SIGNAL_HELPER_MODE="+mode)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatalf("create stdin pipe: %v", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("create stdout pipe: %v", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		t.Fatalf("create stderr pipe: %v", err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start helper: %v", err)
	}
	diagnostics := new(bytes.Buffer)
	ready := make(chan struct{})
	stderrDone := make(chan struct{})
	go func() {
		scanner := bufio.NewScanner(stderr)
		for scanner.Scan() {
			line := scanner.Text()
			diagnostics.WriteString(line)
			diagnostics.WriteByte('\n')
			if line == "DAM_SIGNAL_READY" {
				select {
				case <-ready:
				default:
					close(ready)
				}
			}
		}
		close(stderrDone)
	}()

	output := make(chan byte, 1024)
	go func() {
		buffer := make([]byte, 1)
		for {
			n, readErr := stdout.Read(buffer)
			for i := 0; i < n; i++ {
				output <- buffer[i]
			}
			if readErr != nil {
				close(output)
				return
			}
		}
	}()

	return &signalHelper{cmd: cmd, stdin: stdin, output: output, diagnostics: diagnostics, ready: ready, stderrDone: stderrDone}
}

func (helper *signalHelper) signal(sig os.Signal) error {
	return helper.cmd.Process.Signal(sig)
}

func (helper *signalHelper) finish(t *testing.T) {
	t.Helper()
	if err := helper.stdin.Close(); err != nil {
		t.Fatalf("close helper stdin: %v", err)
	}
	wait := make(chan error, 1)
	go func() { wait <- helper.cmd.Wait() }()
	select {
	case err := <-wait:
		<-helper.stderrDone
		if err != nil {
			t.Fatalf("helper exited unsuccessfully: %v (stderr: %s)", err, helper.diagnostics.String())
		}
	case <-time.After(testTimeout):
		_ = helper.cmd.Process.Kill()
		<-wait
		<-helper.stderrDone
		t.Fatal("helper did not exit after stdin EOF")
	}
}

func (helper *signalHelper) cleanup() {
	_ = helper.stdin.Close()
	if helper.cmd.ProcessState == nil {
		helper.kill()
	}
	<-helper.stderrDone
}

func (helper *signalHelper) kill() {
	_ = helper.cmd.Process.Kill()
	_ = helper.cmd.Wait()
}

func TestSignalHelperProcess(t *testing.T) {
	if os.Getenv("DAM_SIGNAL_HELPER") != "1" {
		return
	}
	os.Args = append([]string{"dam"}, signalHelperArgs(os.Getenv("DAM_SIGNAL_HELPER_MODE"))...)
	var ready func()
	if os.Getenv("DAM_SIGNAL_HELPER_MODE") == "preinput" {
		ready = func() { fmt.Fprintln(os.Stderr, "DAM_SIGNAL_READY") }
	}
	status, _ := executeWithReady(os.Args[1:], os.Stdin, os.Stdout, os.Stderr, ready)
	os.Exit(status)
}

func signalHelperArgs(mode string) []string {
	switch mode {
	case "preinput":
		return []string{"--release-on=signal:USR1"}
	case "duration":
		return []string{"100ms", "--release-on=signal:USR1"}
	case "zero":
		return []string{"0s", "--release-on=signal:USR1"}
	default:
		panic("unknown signal helper mode")
	}
}

func readExactWithTimeout(t *testing.T, reader <-chan byte, size int) string {
	t.Helper()
	result := make(chan []byte, 1)
	go func() {
		buffer := make([]byte, size)
		for i := range buffer {
			value, ok := <-reader
			if !ok {
				result <- nil
				return
			}
			buffer[i] = value
		}
		result <- buffer
	}()
	select {
	case got := <-result:
		if got == nil {
			t.Fatal("helper closed stdout before expected output")
		}
		return string(got)
	case <-time.After(testTimeout):
		t.Fatal("timed out waiting for helper output")
		return ""
	}
}
