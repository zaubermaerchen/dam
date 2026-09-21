//go:build aix || darwin || dragonfly || freebsd || illumos || linux || netbsd || openbsd || solaris

package condition

import (
	"syscall"
	"testing"
	"time"
)

func TestReleaseMonitorContinuesConsumingConfiguredSignalsAfterSelection(t *testing.T) {
	engine := newEngineWithGroups(false, []Group{{Members: []Condition{{Kind: "signal", Source: "SIGUSR1"}}}})
	defer engine.Close()
	monitor, err := newReleaseMonitorWithEngine([]string{"SIGUSR1"}, engine)
	if err != nil {
		t.Fatal(err)
	}
	defer monitor.Close()
	if monitor.signals == nil {
		t.Fatal("configured signal monitor has no input channel")
	}

	monitor.signals <- syscall.SIGUSR1
	select {
	case <-engine.Satisfied():
	case <-time.After(time.Second):
		t.Fatal("configured signal did not satisfy the condition")
	}

	// The signal subscription remains live after selection so a later signal
	// cannot restore its default terminating behavior before process cleanup.
	monitor.signals <- syscall.SIGUSR1
	select {
	case <-time.After(10 * time.Millisecond):
	case <-monitor.done:
		t.Fatal("signal monitor stopped at root selection")
	}
}
