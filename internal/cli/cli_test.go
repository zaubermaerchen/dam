package cli

// This file locks the parser's public plan shape and condition normalization.

import (
	"testing"
	"time"
)

func TestParseBuildsGroupsAndOptions(t *testing.T) {
	plan, err := ParseAt([]string{
		"duration:0s && file:ready",
		"--events-fd=9",
		"--or=signal:USR1",
		"--buffer-size=2K",
	}, time.UTC)
	if err != nil {
		t.Fatalf("ParseAt returned error: %v", err)
	}
	if got, want := len(plan.Groups), 2; got != want {
		t.Fatalf("group count = %d, want %d", got, want)
	}
	if got, want := len(plan.Groups[0].Members), 2; got != want {
		t.Fatalf("first group member count = %d, want %d", got, want)
	}
	if !plan.ImmediateDuration {
		t.Fatal("ImmediateDuration = false, want true")
	}
	if got, want := plan.BufferSize, 2*1024; got != want {
		t.Fatalf("BufferSize = %d, want %d", got, want)
	}
	if plan.EventsFD == nil || *plan.EventsFD != 9 {
		t.Fatalf("EventsFD = %#v, want 9", plan.EventsFD)
	}
	if got, want := plan.Groups[1].Members[0].Source, "SIGUSR1"; got != want {
		t.Fatalf("signal source = %q, want %q", got, want)
	}
}

func TestParseRejectsInvalidOrdering(t *testing.T) {
	for _, args := range [][]string{
		{"--or", "duration:1s"},
		{"duration:1s", "--or"},
		{"duration:1s", "duration:2s"},
	} {
		if _, err := Parse(args); err == nil {
			t.Fatalf("Parse(%q) unexpectedly succeeded", args)
		}
	}
}

func TestParseAtResolvesDatetime(t *testing.T) {
	location := time.FixedZone("test", 9*60*60)
	plan, err := ParseAt([]string{"datetime:2026-12-31T23:59"}, location)
	if err != nil {
		t.Fatalf("ParseAt returned error: %v", err)
	}
	got := plan.Groups[0].Members[0].Deadline
	want := time.Date(2026, time.December, 31, 23, 59, 0, 0, location)
	if !got.Equal(want) {
		t.Fatalf("deadline = %s, want %s", got, want)
	}
}
