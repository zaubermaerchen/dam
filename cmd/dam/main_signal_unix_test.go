//go:build aix || darwin || dragonfly || freebsd || illumos || linux || netbsd || openbsd || solaris

package main

// This file verifies real SIGUSR1/SIGUSR2 wiring in subprocesses so signal
// delivery does not race with other tests running in the parent test process.

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
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

func TestRunSignalSubprocessReleasesHeldDataOnUSR2(t *testing.T) {
	helper := startSignalHelper(t, "usr2-held")
	defer helper.cleanup()

	select {
	case <-helper.ready:
	case <-time.After(testTimeout):
		t.Fatal("helper did not install its signal monitor")
	}
	if _, err := io.WriteString(helper.stdin, "held"); err != nil {
		t.Fatalf("write held input: %v", err)
	}
	select {
	case value := <-helper.output:
		t.Fatalf("held input was released before SIGUSR2: %q", string([]byte{value}))
	case <-time.After(100 * time.Millisecond):
	}
	if err := helper.signal(syscall.SIGUSR2); err != nil {
		t.Fatalf("send SIGUSR2: %v", err)
	}
	if got := readExactWithTimeout(t, helper.output, len("held")); got != "held" {
		t.Fatalf("held output = %q, want %q", got, "held")
	}
	if _, err := io.WriteString(helper.stdin, "after"); err != nil {
		t.Fatalf("write post-release input: %v", err)
	}
	if got := readExactWithTimeout(t, helper.output, len("after")); got != "after" {
		t.Fatalf("post-release output = %q, want %q", got, "after")
	}
	helper.finish(t)
}

func TestRunSignalSubprocessReleasesOnUSR2BeforeFirstInput(t *testing.T) {
	helper := startSignalHelper(t, "usr2-preinput")
	defer helper.cleanup()

	select {
	case <-helper.ready:
	case <-time.After(testTimeout):
		t.Fatal("helper did not install its signal monitor")
	}
	if err := helper.signal(syscall.SIGUSR2); err != nil {
		t.Fatalf("send pre-input SIGUSR2: %v", err)
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
	helper.finish(t)
}

func TestRunSignalAndFileSubprocessRequiresBothLatchedEvents(t *testing.T) {
	helper := startSignalHelper(t, "and-signal-file")
	defer helper.cleanup()

	select {
	case <-helper.ready:
	case <-time.After(testTimeout):
		t.Fatal("helper did not install its signal monitor")
	}
	if _, err := io.WriteString(helper.stdin, "held"); err != nil {
		t.Fatalf("write held input: %v", err)
	}
	select {
	case value := <-helper.output:
		t.Fatalf("compound condition released before either event: %q", string([]byte{value}))
	case <-time.After(100 * time.Millisecond):
	}
	if err := helper.signal(syscall.SIGUSR1); err != nil {
		t.Fatalf("send SIGUSR1: %v", err)
	}
	select {
	case value := <-helper.output:
		t.Fatalf("compound condition released after signal only: %q", string([]byte{value}))
	case <-time.After(100 * time.Millisecond):
	}
	if err := os.WriteFile(helper.file, []byte("ready"), 0o600); err != nil {
		t.Fatalf("create release file: %v", err)
	}
	if got := readExactWithTimeout(t, helper.output, len("held")); got != "held" {
		t.Fatalf("held output = %q, want %q", got, "held")
	}
	helper.finish(t)
}

func TestRunSignalSubprocessSupportsEitherConfiguredSignal(t *testing.T) {
	tests := []struct {
		name    string
		release os.Signal
	}{
		{name: "SIGUSR1", release: syscall.SIGUSR1},
		{name: "SIGUSR2", release: syscall.SIGUSR2},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			helper := startSignalHelper(t, "both-held")
			defer helper.cleanup()

			select {
			case <-helper.ready:
			case <-time.After(testTimeout):
				t.Fatal("helper did not install its signal monitor")
			}
			if _, err := io.WriteString(helper.stdin, "held"); err != nil {
				t.Fatalf("write held input: %v", err)
			}
			select {
			case value := <-helper.output:
				t.Fatalf("held input was released before a configured signal: %q", string([]byte{value}))
			case <-time.After(100 * time.Millisecond):
			}
			if err := helper.signal(test.release); err != nil {
				t.Fatalf("send %s: %v", test.release, err)
			}
			if got := readExactWithTimeout(t, helper.output, len("held")); got != "held" {
				t.Fatalf("held output = %q, want %q", got, "held")
			}
			for _, sig := range []os.Signal{syscall.SIGUSR1, syscall.SIGUSR2} {
				if err := helper.signal(sig); err != nil {
					t.Fatalf("send post-release %s: %v", sig, err)
				}
			}
			if _, err := io.WriteString(helper.stdin, "after"); err != nil {
				t.Fatalf("write post-release input: %v", err)
			}
			if got := readExactWithTimeout(t, helper.output, len("after")); got != "after" {
				t.Fatalf("post-release output = %q, want %q", got, "after")
			}
			helper.finish(t)
		})
	}
}

func TestRunSignalSubprocessUSR2WinsBeforeDuration(t *testing.T) {
	helper := startSignalHelper(t, "long-duration-usr2")
	defer helper.cleanup()

	select {
	case <-helper.ready:
	case <-time.After(testTimeout):
		t.Fatal("helper did not install its signal monitor")
	}
	if _, err := io.WriteString(helper.stdin, "held"); err != nil {
		t.Fatalf("write held input: %v", err)
	}
	select {
	case value := <-helper.output:
		t.Fatalf("held input was released before SIGUSR2: %q", string([]byte{value}))
	case <-time.After(100 * time.Millisecond):
	}
	if err := helper.signal(syscall.SIGUSR2); err != nil {
		t.Fatalf("send SIGUSR2: %v", err)
	}
	if got := readExactWithTimeout(t, helper.output, len("held")); got != "held" {
		t.Fatalf("held output = %q, want %q", got, "held")
	}
	if _, err := io.WriteString(helper.stdin, "after"); err != nil {
		t.Fatalf("write post-release input: %v", err)
	}
	if got := readExactWithTimeout(t, helper.output, len("after")); got != "after" {
		t.Fatalf("post-release output = %q, want %q", got, "after")
	}
	helper.finish(t)
}

func TestRunSignalSubprocessKeepsUSR2TimerWinnerAlive(t *testing.T) {
	helper := startSignalHelper(t, "duration-usr2")
	defer helper.cleanup()

	if _, err := io.WriteString(helper.stdin, "first"); err != nil {
		t.Fatalf("write first input: %v", err)
	}
	if got := readExactWithTimeout(t, helper.output, len("first")); got != "first" {
		t.Fatalf("first output = %q, want %q", got, "first")
	}
	if err := helper.signal(syscall.SIGUSR2); err != nil {
		t.Fatalf("send post-timer SIGUSR2: %v", err)
	}
	if _, err := io.WriteString(helper.stdin, "second"); err != nil {
		t.Fatalf("write second input: %v", err)
	}
	if got := readExactWithTimeout(t, helper.output, len("second")); got != "second" {
		t.Fatalf("second output = %q, want %q", got, "second")
	}
	helper.finish(t)
}

func TestRunSignalSubprocessKeepsUSR2ZeroDelayWinnerAlive(t *testing.T) {
	helper := startSignalHelper(t, "zero-usr2")
	defer helper.cleanup()

	if _, err := io.WriteString(helper.stdin, "first"); err != nil {
		t.Fatalf("write first input: %v", err)
	}
	if got := readExactWithTimeout(t, helper.output, len("first")); got != "first" {
		t.Fatalf("first output = %q, want %q", got, "first")
	}
	if err := helper.signal(syscall.SIGUSR2); err != nil {
		t.Fatalf("send post-zero-delay SIGUSR2: %v", err)
	}
	if _, err := io.WriteString(helper.stdin, "second"); err != nil {
		t.Fatalf("write second input: %v", err)
	}
	if got := readExactWithTimeout(t, helper.output, len("second")); got != "second" {
		t.Fatalf("second output = %q, want %q", got, "second")
	}
	helper.finish(t)
}

func TestRunSignalSubprocessDoesNotReleaseOnUnconfiguredUSRSignal(t *testing.T) {
	tests := []struct {
		mode       string
		configured syscall.Signal
		ignored    syscall.Signal
	}{
		{mode: "usr2-only", configured: syscall.SIGUSR2, ignored: syscall.SIGUSR1},
		{mode: "usr1-only", configured: syscall.SIGUSR1, ignored: syscall.SIGUSR2},
	}
	for _, test := range tests {
		t.Run(test.mode, func(t *testing.T) {
			helper := startSignalHelper(t, test.mode)
			defer helper.cleanup()

			select {
			case <-helper.ready:
			case <-time.After(testTimeout):
				t.Fatal("helper did not install its signal monitor")
			}
			if err := helper.signal(test.ignored); err != nil {
				t.Fatalf("send unconfigured %s: %v", test.ignored, err)
			}
			if _, err := io.WriteString(helper.stdin, "held"); err != nil {
				t.Fatalf("write held input: %v", err)
			}
			select {
			case value := <-helper.output:
				t.Fatalf("unconfigured %s released input: %q", test.ignored, string([]byte{value}))
			case <-time.After(100 * time.Millisecond):
			}
			if err := helper.signal(test.configured); err != nil {
				t.Fatalf("send configured %s: %v", test.configured, err)
			}
			if got := readExactWithTimeout(t, helper.output, len("held")); got != "held" {
				t.Fatalf("held output = %q, want %q", got, "held")
			}
			helper.finish(t)
		})
	}
}

func TestReleaseMonitorResolvesAndDeduplicatesConfiguredSignals(t *testing.T) {
	signals, err := resolveReleaseSignals([]string{"SIGUSR2", "SIGUSR1", "SIGUSR2"})
	if err != nil {
		t.Fatalf("resolveReleaseSignals returned error: %v", err)
	}
	if got, want := signals, []os.Signal{syscall.SIGUSR2, syscall.SIGUSR1}; !reflect.DeepEqual(got, want) {
		t.Fatalf("signals = %v, want %v", got, want)
	}
}

func TestReleaseMonitorRejectsUnknownCanonicalSignal(t *testing.T) {
	if _, err := resolveReleaseSignals([]string{"SIGUSR3"}); err == nil {
		t.Fatal("resolveReleaseSignals unexpectedly succeeded")
	}
}

func TestReleaseMonitorUsesUniqueSignalCapacity(t *testing.T) {
	monitor, err := newReleaseMonitor([]string{"SIGUSR2", "SIGUSR1", "SIGUSR2"})
	if err != nil {
		t.Fatalf("newReleaseMonitor returned error: %v", err)
	}
	t.Cleanup(monitor.Close)
	if got, want := cap(monitor.signals), 2; got != want {
		t.Fatalf("signal channel capacity = %d, want %d", got, want)
	}
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
	file        string
}

func startSignalHelper(t *testing.T, mode string) *signalHelper {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=TestSignalHelperProcess")
	cmd.Env = append(os.Environ(), "DAM_SIGNAL_HELPER=1", "DAM_SIGNAL_HELPER_MODE="+mode)
	file := ""
	if mode == "and-signal-file" || mode == "mixed-all" {
		file = filepath.Join(t.TempDir(), "ready")
		cmd.Env = append(cmd.Env, "DAM_SIGNAL_HELPER_FILE="+file)
	}
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

	return &signalHelper{cmd: cmd, stdin: stdin, output: output, diagnostics: diagnostics, ready: ready, stderrDone: stderrDone, file: file}
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
	if os.Getenv("DAM_SIGNAL_HELPER_MODE") != "" {
		ready = func() { fmt.Fprintln(os.Stderr, "DAM_SIGNAL_READY") }
	}
	status, _ := executeWithReady(os.Args[1:], os.Stdin, os.Stdout, os.Stderr, ready)
	os.Exit(status)
}

func signalHelperArgs(mode string) []string {
	switch mode {
	case "preinput":
		return []string{"signal:USR1"}
	case "usr2-preinput", "usr2-held", "usr2-only":
		return []string{"signal:USR2"}
	case "usr1-only":
		return []string{"signal:USR1"}
	case "both-held":
		return []string{"signal:USR1", "--or", "signal:USR2"}
	case "duration":
		return []string{"duration:100ms", "--or", "signal:USR1"}
	case "zero":
		return []string{"duration:0s", "--or", "signal:USR1"}
	case "duration-usr2":
		return []string{"duration:100ms", "--or", "signal:USR2"}
	case "long-duration-usr2":
		return []string{"duration:1h", "--or", "signal:USR2"}
	case "zero-usr2":
		return []string{"duration:0s", "--or", "signal:USR2"}
	case "duration-and-signal":
		return []string{"duration:0s && signal:USR1"}
	case "mixed-all":
		deadline := time.Now().Add(time.Hour).Format("2006-01-02T15:04:05")
		return []string{
			"duration:1h && datetime:" + deadline + " && signal:USR1 && file:" + os.Getenv("DAM_SIGNAL_HELPER_FILE"),
			"--or",
			"signal:USR2",
		}
	case "and-signal-file":
		return []string{"signal:USR1 && file:" + os.Getenv("DAM_SIGNAL_HELPER_FILE")}
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
