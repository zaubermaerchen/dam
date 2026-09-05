//go:build windows || (!aix && !darwin && !dragonfly && !freebsd && !illumos && !linux && !netbsd && !openbsd && !solaris)

package main

// This file verifies that targets without Unix user-signal support reject
// every signal-bearing expression while retaining timed and file conditions.

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRunRejectsEverySignalBearingExpressionOnUnsupportedPlatforms(t *testing.T) {
	for _, args := range [][]string{
		{"signal:USR1"},
		{"signal:SIGUSR1"},
		{"signal:USR2"},
		{"signal:SIGUSR2"},
		{"duration:1s && signal:USR1"},
		{"file:pending && signal:SIGUSR2"},
		{"duration:1s", "--or", "signal:USR1"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			input := &trackingReader{}
			var output, diagnostics bytes.Buffer
			if status := run(args, input, &output, &diagnostics); status == 0 {
				t.Fatal("signal-bearing expression unexpectedly succeeded")
			}
			if input.reads != 0 {
				t.Fatalf("unsupported signal read stdin %d times", input.reads)
			}
			if output.Len() != 0 {
				t.Fatalf("unsupported signal wrote stdout: %q", output.String())
			}
			if !strings.Contains(diagnostics.String(), "not supported") {
				t.Fatalf("diagnostic = %q, want unsupported-platform error", diagnostics.String())
			}
		})
	}
}

func TestRunAcceptsTimedAndFileOnlyExpressionsOnUnsupportedPlatforms(t *testing.T) {
	dir := t.TempDir()
	ready := filepath.Join(dir, "ready")
	if err := os.WriteFile(ready, []byte("ready"), 0o600); err != nil {
		t.Fatalf("create regular release file: %v", err)
	}

	for _, test := range []struct {
		name  string
		args  []string
		input string
		want  string
	}{
		{name: "duration", args: []string{"duration:0s"}, input: "duration", want: "duration"},
		{name: "past datetime", args: []string{"datetime:2000-01-01T00:00:00"}, input: "datetime", want: "datetime"},
		{name: "file", args: []string{"file:" + ready}, input: "file", want: "file"},
		{name: "timed or file", args: []string{"duration:0s", "--or", "file:" + ready}, input: "mixed", want: "mixed"},
	} {
		t.Run(test.name, func(t *testing.T) {
			var output, diagnostics bytes.Buffer
			if status := run(test.args, strings.NewReader(test.input), &output, &diagnostics); status != 0 {
				t.Fatalf("run status = %d, diagnostics = %q", status, diagnostics.String())
			}
			if got := output.String(); got != test.want {
				t.Fatalf("output = %q, want %q", got, test.want)
			}
			if diagnostics.Len() != 0 {
				t.Fatalf("successful run wrote diagnostics: %q", diagnostics.String())
			}
		})
	}
}
