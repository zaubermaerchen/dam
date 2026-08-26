//go:build windows

package main

// This file verifies that Windows keeps the signal option explicit instead of
// silently accepting a release condition it cannot implement.

import (
	"bytes"
	"strings"
	"testing"
)

func TestRunRejectsSignalReleaseOnWindows(t *testing.T) {
	var output, diagnostics bytes.Buffer
	if status := run([]string{"--release-on=signal:USR1"}, strings.NewReader("input"), &output, &diagnostics); status == 0 {
		t.Fatal("signal release unexpectedly succeeded on Windows")
	}
	if output.Len() != 0 {
		t.Fatalf("unsupported signal wrote stdout: %q", output.String())
	}
	if !strings.Contains(diagnostics.String(), "not supported") {
		t.Fatalf("diagnostic = %q, want unsupported-platform error", diagnostics.String())
	}
}
