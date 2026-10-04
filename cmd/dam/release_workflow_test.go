package main

// This file checks the release workflow's Homebrew handoff and stable-tag gate.

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestReleaseWorkflowHomebrewHandoff(t *testing.T) {
	workflow, err := os.ReadFile(filepath.Join("..", "..", ".github", "workflows", "release.yml"))
	if err != nil {
		t.Fatal(err)
	}
	content := string(workflow)
	for _, required := range []string{
		"homebrew-eligible: ${{ steps.homebrew-eligibility.outputs.homebrew-eligible }}",
		"needs.release.outputs.homebrew-eligible == 'true'",
		"uses: zaubermaerchen/homebrew-tap/.github/workflows/update-formula.yml@18a30289f199e21a6ddd1d5be1b57f6249fe0622",
		"automation-ref: 18a30289f199e21a6ddd1d5be1b57f6249fe0622",
		"tag: ${{ needs.prepare.outputs.version }}",
		"formula: dam",
		"app-id: ${{ vars.HOMEBREW_TAP_APP_ID }}",
		"app-private-key: ${{ secrets.HOMEBREW_TAP_APP_PRIVATE_KEY }}",
	} {
		if !strings.Contains(content, required) {
			t.Errorf("release workflow is missing %q", required)
		}
	}

	_, step, ok := strings.Cut(content, "      - name: Select Homebrew update\n        id: homebrew-eligibility\n        shell: bash\n        run: |\n")
	if !ok {
		t.Fatal("Homebrew eligibility step is missing")
	}
	step, _, ok = strings.Cut(step, "\n\n  homebrew:")
	if !ok {
		t.Fatal("Homebrew job is missing after the eligibility step")
	}
	if runtime.GOOS == "windows" {
		t.Skip("bash output paths are platform-specific on Windows")
	}
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash is unavailable for running the release step")
	}
	var script strings.Builder
	for _, line := range strings.Split(step, "\n") {
		if !strings.HasPrefix(line, "          ") {
			t.Fatalf("unexpected eligibility script line: %q", line)
		}
		script.WriteString(strings.TrimPrefix(line, "          "))
		script.WriteByte('\n')
	}
	for _, tc := range []struct {
		tag  string
		want string
	}{
		{"v0.5.0", "true"},
		{"v10.20.30", "true"},
		{"v1.2.3-rc.1", "false"},
		{"v1.2.3+build.1", "false"},
		{"v01.2.3", "false"},
	} {
		t.Run(tc.tag, func(t *testing.T) {
			outputPath := filepath.Join(t.TempDir(), "github-output")
			cmd := exec.Command("bash", "-e", "-c", script.String())
			cmd.Env = append(os.Environ(), "VERSION="+tc.tag, "GITHUB_OUTPUT="+outputPath)
			if output, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("eligibility step failed: %v: %s", err, output)
			}
			output, err := os.ReadFile(outputPath)
			if err != nil {
				t.Fatal(err)
			}
			if got := string(output); got != "homebrew-eligible="+tc.want+"\n" {
				t.Errorf("eligibility for %q = %q, want %q", tc.tag, got, tc.want)
			}
		})
	}
}
