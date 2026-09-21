package cli

// This file keeps command-line syntax, datetime resolution, and option validation
// tests next to the parser implementation.

import (
	"bytes"
	"encoding/binary"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"
)

const preReleaseBufferSize = defaultBufferSize

type releaseCondition struct {
	kind     string
	source   string
	duration time.Duration
	deadline time.Time
}

type releaseGroup struct {
	members []releaseCondition
}

type testConfig struct {
	delay             *time.Duration
	deadline          *time.Time
	signals           []string
	files             []string
	groups            []releaseGroup
	eventsFD          *int
	bufferSize        int
	immediateDuration bool
}

func newDurationReleaseCondition(value time.Duration) releaseCondition {
	return releaseCondition{kind: "duration", source: value.String(), duration: value}
}

func newDatetimeReleaseCondition(value time.Time) releaseCondition {
	return releaseCondition{kind: "datetime", source: value.UTC().Format(time.RFC3339Nano), deadline: value}
}

func parseConfig(args []string) (testConfig, error) {
	return parseConfigAt(args, time.Local)
}

func parseConfigAt(args []string, location *time.Location) (testConfig, error) {
	plan, err := ParseAt(args, location)
	if err != nil {
		return testConfig{}, err
	}
	config := testConfig{bufferSize: plan.BufferSize, immediateDuration: plan.ImmediateDuration}
	if plan.EventsFD != nil {
		fd := *plan.EventsFD
		config.eventsFD = &fd
	}
	for _, group := range plan.Groups {
		runtimeGroup := releaseGroup{members: make([]releaseCondition, 0, len(group.Members))}
		for _, condition := range group.Members {
			runtimeGroup.members = append(runtimeGroup.members, releaseCondition{
				kind: condition.Kind, source: condition.Source,
				duration: condition.Duration, deadline: condition.Deadline,
			})
			switch condition.Kind {
			case "signal":
				config.signals = append(config.signals, condition.Source)
			case "file":
				config.files = append(config.files, condition.Source)
			case "duration":
				if config.delay == nil {
					value := condition.Duration
					config.delay = &value
				}
			case "datetime":
				if config.deadline == nil {
					value := condition.Deadline
					config.deadline = &value
				}
			}
		}
		config.groups = append(config.groups, runtimeGroup)
	}
	return config, nil
}

func equalStrings(got, want []string) bool {
	return reflect.DeepEqual(got, want)
}

func TestParseConfigAcceptsDurationAndRepeatableSignalsInAnyOrder(t *testing.T) {
	config, err := parseConfig([]string{
		"signal:USR1",
		"--or",
		"duration:250ms",
		"--or",
		"signal:SIGUSR1",
	})
	if err != nil {
		t.Fatalf("parseConfig returned error: %v", err)
	}
	if config.delay == nil || *config.delay != 250*time.Millisecond {
		t.Fatalf("delay = %v, want 250ms", config.delay)
	}
	if got, want := config.signals, []string{"SIGUSR1", "SIGUSR1"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("signals = %v, want %v", got, want)
	}
}

func TestParseConfigAcceptsSignalWithoutDuration(t *testing.T) {
	config, err := parseConfig([]string{"signal:SIGUSR1"})
	if err != nil {
		t.Fatalf("parseConfig returned error: %v", err)
	}
	if config.delay != nil {
		t.Fatalf("delay = %v, want nil", config.delay)
	}
	if got, want := config.signals, []string{"SIGUSR1"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("signals = %v, want %v", got, want)
	}
}

func TestParseConfigAcceptsAbsoluteLocalDeadline(t *testing.T) {
	location := time.FixedZone("test", 9*60*60)
	config, err := parseConfigAt([]string{"datetime:2026-12-31T23:59"}, location)
	if err != nil {
		t.Fatalf("parseConfigAt returned error: %v", err)
	}
	if config.delay != nil {
		t.Fatalf("delay = %v, want nil", config.delay)
	}
	if config.deadline == nil {
		t.Fatal("deadline = nil, want absolute deadline")
	}
	want := time.Date(2026, time.December, 31, 23, 59, 0, 0, location)
	if !config.deadline.Equal(want) {
		t.Fatalf("deadline = %s, want %s", config.deadline, want)
	}
}

func TestParseConfigAcceptsAbsoluteDeadlineWithSeconds(t *testing.T) {
	config, err := parseConfigAt([]string{"datetime:2026-12-31T23:59:07"}, time.UTC)
	if err != nil {
		t.Fatalf("parseConfigAt returned error: %v", err)
	}
	if config.deadline == nil {
		t.Fatal("deadline = nil, want absolute deadline")
	}
	if got, want := config.deadline.Second(), 7; got != want {
		t.Fatalf("deadline second = %d, want %d", got, want)
	}
}

func TestParseAbsoluteDeadlineAcceptsExplicitRFC3339Timezones(t *testing.T) {
	location := time.FixedZone("caller", -5*60*60)
	for _, tc := range []struct {
		value      string
		want       time.Time
		wantOffset int
	}{
		{value: "2026-09-06T00:00:00Z", want: time.Date(2026, time.September, 6, 0, 0, 0, 0, time.UTC), wantOffset: 0},
		{value: "2026-09-06T09:00:00+09:00", want: time.Date(2026, time.September, 6, 9, 0, 0, 0, time.FixedZone("+09:00", 9*60*60)), wantOffset: 9 * 60 * 60},
		{value: "2026-09-06T09:00:00-04:30", want: time.Date(2026, time.September, 6, 9, 0, 0, 0, time.FixedZone("-04:30", -4*60*60-30*60)), wantOffset: -4*60*60 - 30*60},
	} {
		t.Run(tc.value, func(t *testing.T) {
			got, err := parseAbsoluteDeadline(tc.value, location)
			if err != nil {
				t.Fatalf("parseAbsoluteDeadline returned error: %v", err)
			}
			if !got.Equal(tc.want) {
				t.Fatalf("deadline = %s (%s), want %s (%s)", got, got.UTC(), tc.want, tc.want.UTC())
			}
			if _, offset := got.Zone(); offset != tc.wantOffset {
				t.Fatalf("timezone offset = %d, want %d", offset, tc.wantOffset)
			}
		})
	}
}

func TestParseAbsoluteDeadlineRejectsMalformedNumericOffsetWithASCIIHint(t *testing.T) {
	_, err := parseAbsoluteDeadline("2026-09-03T18:00:00+09:0X", time.UTC)
	if err == nil {
		t.Fatal("parseAbsoluteDeadline unexpectedly accepted malformed numeric offset")
	}
	const want = "datetime timezone offset must use +HH:MM or -HH:MM"
	if got := err.Error(); got != want {
		t.Fatalf("error = %q, want %q", got, want)
	}
}

func TestParseConfigAcceptsAbsoluteDeadlineYearBoundaries(t *testing.T) {
	for _, value := range []string{"0001-01-01T00:00", "9999-12-31T23:59:59"} {
		t.Run(value, func(t *testing.T) {
			config, err := parseConfigAt([]string{"datetime:" + value}, time.UTC)
			if err != nil {
				t.Fatalf("parseConfigAt returned error: %v", err)
			}
			if config.deadline == nil {
				t.Fatal("deadline = nil, want absolute deadline")
			}
		})
	}
}

func TestParseConfigRejectsMalformedAbsoluteDeadlines(t *testing.T) {
	for _, value := range []string{
		"0000-01-01T00:00",
		"2026-02-29T12:00",
		"2026-12-31T24:00",
		"2026-12-31T23:59:60",
		"2026-12-31t23:59",
		"2026-12-31 23:59",
		"2026-12-31T23:xx",
		"2026-12-31T23:59.1",
		"2026-12-31T23:59Z",
		"2026-12-31T23:59+09:00",
		"2026-12-31T23:59:00.1Z",
		"2026-12-31T23:59:00.123+09:00",
		"2026-12-31T23:59:00z",
		"2026-12-31T23:59:00+09",
		"2026-12-31T23:59:00+0900",
		"2026-12-31T23:59:00+24:00",
		"2026-12-31T23:59:00+09:60",
		"2026-12-31T23:59:00UTC",
		"2026-12-31T23:59:00Asia/Tokyo",
		"0000-01-01T00:00:00Z",
		"2026-12-31T23:59UTC",
	} {
		t.Run(value, func(t *testing.T) {
			if _, err := parseConfigAt([]string{"datetime:" + value}, time.UTC); err == nil {
				t.Fatal("parseConfigAt unexpectedly succeeded")
			}
		})
	}
}

func TestParseConfigAcceptsMultipleKindsOfTimedConditions(t *testing.T) {
	config, err := parseConfigAt([]string{
		"duration:1s",
		"--or",
		"datetime:2026-12-31T23:59",
		"--or=datetime:2027-01-01T00:00",
	}, time.UTC)
	if err != nil {
		t.Fatalf("parseConfigAt returned error: %v", err)
	}
	if len(config.groups) != 3 {
		t.Fatalf("groups = %#v, want three alternatives", config.groups)
	}
}

func TestParseAbsoluteDeadlineUsesEarlierInstantForDSTOverlap(t *testing.T) {
	location, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Skipf("load timezone: %v", err)
	}
	got, err := parseAbsoluteDeadline("2024-11-03T01:30", location)
	if err != nil {
		t.Fatalf("parseAbsoluteDeadline returned error: %v", err)
	}
	want := time.Date(2024, time.November, 3, 1, 30, 0, 0, time.FixedZone("EDT", -4*60*60))
	if !got.Equal(want) {
		t.Fatalf("deadline = %s (%s), want %s (%s)", got, got.UTC(), want, want.UTC())
	}
}

func TestEarliestLocalInstantFindsShortLivedHistoricalOffset(t *testing.T) {
	location, err := time.LoadLocationFromTZData("Synthetic/Short", shortLivedOffsetTZif(t))
	if err != nil {
		t.Fatalf("load synthetic timezone: %v", err)
	}
	parsed := time.Unix(30, 0).In(location)
	if !sameLocalDateTime(parsed, 1970, time.January, 1, 0, 0, 0) {
		t.Fatalf("later occurrence = %s, want 1970-01-01T00:00:00", parsed)
	}

	got := earliestLocalInstant(parsed, 1970, time.January, 1, 0, 0, 0, location)
	want := time.Unix(0, 0).In(location)
	if !got.Equal(want) {
		t.Fatalf("earliest occurrence = %s (%s), want %s (%s)", got, got.UTC(), want, want.UTC())
	}
}

func TestParseAbsoluteDeadlineHandlesTransitionAtZeroTime(t *testing.T) {
	location, err := time.LoadLocationFromTZData("Synthetic/Zero", zeroTimeTransitionTZif(t))
	if err != nil {
		t.Fatalf("load synthetic timezone: %v", err)
	}

	got, err := parseAbsoluteDeadline("0001-01-01T00:00:00", location)
	if err != nil {
		t.Fatalf("parseAbsoluteDeadline returned error: %v", err)
	}
	want := time.Date(1, time.January, 1, 0, 0, -30, 0, time.UTC).In(location)
	if !got.Equal(want) {
		t.Fatalf("earliest occurrence = %s (%s), want %s (%s)", got, got.UTC(), want, want.UTC())
	}
}

func shortLivedOffsetTZif(t *testing.T) []byte {
	t.Helper()
	var data bytes.Buffer
	data.WriteString("TZif")
	data.Write(make([]byte, 16))
	for _, count := range []uint32{0, 0, 0, 3, 4, 8} {
		if err := binary.Write(&data, binary.BigEndian, count); err != nil {
			t.Fatalf("write TZif header: %v", err)
		}
	}
	for _, transition := range []int32{-20, 10, 20} {
		if err := binary.Write(&data, binary.BigEndian, transition); err != nil {
			t.Fatalf("write TZif transition: %v", err)
		}
	}
	data.Write([]byte{1, 2, 3})
	for _, zone := range []struct {
		offset int32
		name   byte
	}{
		{offset: -7200, name: 0},
		{offset: 0, name: 2},
		{offset: 36000, name: 4},
		{offset: -30, name: 6},
	} {
		if err := binary.Write(&data, binary.BigEndian, zone.offset); err != nil {
			t.Fatalf("write TZif offset: %v", err)
		}
		data.WriteByte(0)
		data.WriteByte(zone.name)
	}
	data.WriteString("D\x00A\x00B\x00C\x00")
	return data.Bytes()
}

func zeroTimeTransitionTZif(t *testing.T) []byte {
	t.Helper()
	var data bytes.Buffer
	writeTZifHeader(t, &data, '2', 0, 1, 2)
	writeTZifZone(t, &data, 0, 0)
	data.WriteString("X\x00")

	writeTZifHeader(t, &data, '2', 1, 2, 4)
	if err := binary.Write(&data, binary.BigEndian, int64(-62135596800)); err != nil {
		t.Fatalf("write TZif transition: %v", err)
	}
	data.WriteByte(1)
	writeTZifZone(t, &data, 30, 0)
	writeTZifZone(t, &data, 0, 2)
	data.WriteString("A\x00B\x00")
	data.WriteString("\n\n")
	return data.Bytes()
}

func writeTZifHeader(t *testing.T, data *bytes.Buffer, version byte, transitions, zones, names uint32) {
	t.Helper()
	data.WriteString("TZif")
	data.WriteByte(version)
	data.Write(make([]byte, 15))
	for _, count := range []uint32{0, 0, 0, transitions, zones, names} {
		if err := binary.Write(data, binary.BigEndian, count); err != nil {
			t.Fatalf("write TZif header: %v", err)
		}
	}
}

func writeTZifZone(t *testing.T, data *bytes.Buffer, offset int32, name byte) {
	t.Helper()
	if err := binary.Write(data, binary.BigEndian, offset); err != nil {
		t.Fatalf("write TZif offset: %v", err)
	}
	data.WriteByte(0)
	data.WriteByte(name)
}

func TestParseAbsoluteDeadlineRejectsDSTGap(t *testing.T) {
	location, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Skipf("load timezone: %v", err)
	}
	if _, err := parseAbsoluteDeadline("2024-03-10T02:30", location); err == nil {
		t.Fatal("parseAbsoluteDeadline unexpectedly accepted a DST gap")
	}
}

func TestParseConfigAcceptsUSR2AliasesAndPreservesOrder(t *testing.T) {
	config, err := parseConfig([]string{
		"signal:USR2",
		"--or",
		"duration:250ms",
		"--or",
		"signal:SIGUSR2",
	})
	if err != nil {
		t.Fatalf("parseConfig returned error: %v", err)
	}
	if config.delay == nil || *config.delay != 250*time.Millisecond {
		t.Fatalf("delay = %v, want 250ms", config.delay)
	}
	if got, want := config.signals, []string{"SIGUSR2", "SIGUSR2"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("signals = %v, want %v", got, want)
	}
}

func TestParseConfigDefaultsBufferSize(t *testing.T) {
	config, err := parseConfig([]string{"duration:1s"})
	if err != nil {
		t.Fatalf("parseConfig returned error: %v", err)
	}
	if got, want := config.bufferSize, preReleaseBufferSize; got != want {
		t.Fatalf("buffer size = %d, want default %d", got, want)
	}
}

func TestParseConfigAcceptsBufferSizeFormsAndBinaryUnits(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want int
	}{
		{name: "bytes separated", args: []string{"duration:1s", "--buffer-size", "123"}, want: 123},
		{name: "bytes equals", args: []string{"--buffer-size=123", "duration:1s"}, want: 123},
		{name: "upper kilobytes", args: []string{"duration:1s", "--buffer-size", "2K"}, want: 2 * 1024},
		{name: "lower kilobytes", args: []string{"duration:1s", "--buffer-size=1k"}, want: 1024},
		{name: "upper megabytes", args: []string{"duration:1s", "--buffer-size=3M"}, want: 3 * 1024 * 1024},
		{name: "lower megabytes", args: []string{"--buffer-size", "4m", "duration:1s"}, want: 4 * 1024 * 1024},
		{name: "upper gigabytes", args: []string{"duration:1s", "--buffer-size=1G"}, want: 1024 * 1024 * 1024},
		{name: "lower gigabytes", args: []string{"--buffer-size", "1g", "duration:1s"}, want: 1024 * 1024 * 1024},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			config, err := parseConfig(test.args)
			if err != nil {
				t.Fatalf("parseConfig returned error: %v", err)
			}
			if config.bufferSize != test.want {
				t.Fatalf("buffer size = %d, want %d", config.bufferSize, test.want)
			}
		})
	}
}

func TestParseConfigRejectsInvalidBufferSizes(t *testing.T) {
	for _, value := range []string{
		"",
		"0",
		"-1",
		"+1",
		"1.5",
		"1KB",
		"1KiB",
		"1KK",
		"1T",
		"18446744073709551615",
		"18446744073709551615G",
	} {
		t.Run(value, func(t *testing.T) {
			args := []string{"duration:1s", "--buffer-size", value}
			if _, err := parseConfig(args); err == nil {
				t.Fatal("parseConfig unexpectedly succeeded")
			}
		})
	}

	for _, args := range [][]string{
		{"duration:1s", "--buffer-size"},
		{"duration:1s", "--buffer-size="},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			if _, err := parseConfig(args); err == nil {
				t.Fatal("parseConfig unexpectedly succeeded")
			}
		})
	}
}

func TestParseConfigBufferSizeAloneIsNotAReleaseCondition(t *testing.T) {
	if _, err := parseConfig([]string{"--buffer-size", "1K"}); err == nil {
		t.Fatal("buffer-size without a release condition unexpectedly succeeded")
	}
}

func TestParseBufferSize32BitBoundaries(t *testing.T) {
	if strconv.IntSize != 32 {
		t.Skip("32-bit integer boundary test")
	}

	for _, value := range []string{"2147483648", "2G", "3G"} {
		t.Run(value, func(t *testing.T) {
			if _, err := parseBufferSize(value); err == nil {
				t.Fatalf("parseBufferSize(%q) unexpectedly succeeded", value)
			}
		})
	}
	if got, err := parseBufferSize("2147483647"); err != nil || got != 2147483647 {
		t.Fatalf("parseBufferSize(2147483647) = %d, %v", got, err)
	}
}

func TestParseConfigRejectsInvalidUSR2Conditions(t *testing.T) {
	for _, value := range []string{
		"signal:usr2",
		"signal:SigUSR2",
		"signal:SIGUSR2:extra",
	} {
		t.Run(value, func(t *testing.T) {
			if _, err := parseConfig([]string{value}); err == nil {
				t.Fatal("parseConfig unexpectedly succeeded")
			}
		})
	}
}

func TestParseConfigDistinguishesAdditionalPositionalArguments(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want string
	}{
		{
			name: "second duration",
			args: []string{"duration:1s", "duration:2s"},
			want: `unexpected argument "duration:2s"`,
		},
		{
			name: "unexpected argument",
			args: []string{"duration:1s", "foo"},
			want: `unexpected argument "foo"`,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := parseConfig(test.args)
			if err == nil {
				t.Fatal("parseConfig unexpectedly succeeded")
			}
			if got := err.Error(); !strings.Contains(got, test.want) {
				t.Fatalf("parseConfig error = %q, want substring %q", got, test.want)
			}
		})
	}
}

func TestParseConfigRejectsInvalidReleaseConditions(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want string
	}{
		{name: "missing all", args: nil, want: "usage: dam CONDITION [--or CONDITION]... [--buffer-size SIZE]"},
		{name: "duplicate duration", args: []string{"duration:1s", "duration:2s"}},
		{name: "missing release value", args: []string{"--release-on"}},
		{name: "empty release value", args: []string{"--release-on="}},
		{name: "missing source", args: []string{"signal:"}},
		{name: "unknown type", args: []string{"term:TERM"}},
		{name: "unknown signal", args: []string{"signal:TERM"}},
		{name: "lowercase type", args: []string{"Signal:USR1"}},
		{name: "lowercase source", args: []string{"signal:usr1"}},
		{name: "extra separator", args: []string{"signal:USR1:extra"}},
		{name: "version with release", args: []string{"--version", "--release-on", "signal:USR1"}},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := parseConfig(test.args)
			if err == nil {
				t.Fatal("parseConfig unexpectedly succeeded")
			}
			if test.want != "" && err.Error() != test.want {
				t.Fatalf("parseConfig error = %q, want %q", err, test.want)
			}
		})
	}
}

func TestParseConfigBuildsCompoundReleaseGroupsInConfigurationOrder(t *testing.T) {
	config, err := parseConfig([]string{
		"signal:USR1 && file:first",
		"--or",
		"duration:250ms",
		"--or=file:second && signal:SIGUSR2",
	})
	if err != nil {
		t.Fatalf("parseConfig returned error: %v", err)
	}
	want := []releaseGroup{
		{members: []releaseCondition{
			{kind: "signal", source: "SIGUSR1"},
			{kind: "file", source: "first"},
		}},
		{members: []releaseCondition{newDurationReleaseCondition(250 * time.Millisecond)}},
		{members: []releaseCondition{
			{kind: "file", source: "second"},
			{kind: "signal", source: "SIGUSR2"},
		}},
	}
	if !reflect.DeepEqual(config.groups, want) {
		t.Fatalf("groups = %#v, want %#v", config.groups, want)
	}
	if got, want := config.signals, []string{"SIGUSR1", "SIGUSR2"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("signals = %v, want %v", got, want)
	}
	if got, want := config.files, []string{"first", "second"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("files = %v, want %v", got, want)
	}
	if config.delay == nil || *config.delay != 250*time.Millisecond {
		t.Fatalf("delay = %v, want 250ms", config.delay)
	}
}

func TestParseConfigAcceptsPositionalConditionsAndExplicitOR(t *testing.T) {
	config, err := parseConfigAt([]string{
		"--buffer-size=1K",
		"duration:250ms",
		"--or",
		"datetime:2026-12-31T23:59:07",
		"--buffer-size",
		"2K",
		"--or=signal:SIGUSR1 && file:ready",
	}, time.UTC)
	if err != nil {
		t.Fatalf("parseConfig returned error: %v", err)
	}
	want := []releaseGroup{
		{members: []releaseCondition{newDurationReleaseCondition(250 * time.Millisecond)}},
		{members: []releaseCondition{newDatetimeReleaseCondition(time.Date(2026, time.December, 31, 23, 59, 7, 0, time.UTC))}},
		{members: []releaseCondition{
			{kind: "signal", source: "SIGUSR1"},
			{kind: "file", source: "ready"},
		}},
	}
	if !reflect.DeepEqual(config.groups, want) {
		t.Fatalf("groups = %#v, want %#v", config.groups, want)
	}
	if got, want := config.signals, []string{"SIGUSR1"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("signals = %v, want %v", got, want)
	}
	if got, want := config.files, []string{"ready"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("files = %v, want %v", got, want)
	}
	if got, want := config.bufferSize, 2*1024; got != want {
		t.Fatalf("buffer size = %d, want %d", got, want)
	}
}

func TestParseConfigAcceptsTimedMembersInsideANDGroups(t *testing.T) {
	config, err := parseConfig([]string{
		"duration:1s && datetime:2026-12-31T23:59",
		"--or=signal:USR2 && duration:2s",
	})
	if err != nil {
		t.Fatalf("parseConfig returned error: %v", err)
	}
	if len(config.groups) != 2 || len(config.groups[0].members) != 2 || len(config.groups[1].members) != 2 {
		t.Fatalf("groups = %#v, want two two-member groups", config.groups)
	}
	if got, want := config.groups[0].members[0].kind, "duration"; got != want {
		t.Fatalf("first group first member kind = %q, want %q", got, want)
	}
	if got, want := config.groups[0].members[1].kind, "datetime"; got != want {
		t.Fatalf("first group second member kind = %q, want %q", got, want)
	}
	if got, want := config.groups[1].members[0].source, "SIGUSR2"; got != want {
		t.Fatalf("second group first member source = %q, want %q", got, want)
	}
	if got, want := config.groups[1].members[1].duration, 2*time.Second; got != want {
		t.Fatalf("second group second member duration = %s, want %s", got, want)
	}
}

func TestParseConfigTracksImmediateDurationRegardlessOfConditionOrder(t *testing.T) {
	config, err := parseConfig([]string{"signal:USR1", "--or", "duration:1s", "--or", "duration:0s"})
	if err != nil {
		t.Fatalf("parseConfig returned error: %v", err)
	}
	if !config.immediateDuration {
		t.Fatal("config did not track an immediate duration")
	}
}

func TestParseConfigRejectsMissingOrAdjacentORConditionsAndRemovedSyntax(t *testing.T) {
	tests := [][]string{
		{"--or", "signal:USR1"},
		{"signal:USR1", "--or"},
		{"signal:USR1", "--or="},
		{"signal:USR1", "--or", "--buffer-size", "1K"},
		{"signal:USR1", "file:ready"},
		{"--release-on", "signal:USR1"},
		{"--release-on=signal:USR1"},
		{"1s"},
		{"2026-12-31T23:59"},
	}
	for _, args := range tests {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			if _, err := parseConfig(args); err == nil {
				t.Fatal("parseConfig unexpectedly accepted removed or incomplete syntax")
			}
		})
	}
}

func TestParseConfigCompoundReleaseUsesExactSeparatorWithoutTrimming(t *testing.T) {
	config, err := parseConfig([]string{"file:ready  && signal:USR1"})
	if err != nil {
		t.Fatalf("parseConfig returned error: %v", err)
	}
	if got, want := config.groups[0].members[0].source, "ready "; got != want {
		t.Fatalf("first file source = %q, want %q", got, want)
	}
	pathConfig, err := parseConfig([]string{"file:a&&b"})
	if err != nil {
		t.Fatalf("parseConfig rejected non-separator ampersands: %v", err)
	}
	if got, want := pathConfig.groups[0].members[0].source, "a&&b"; got != want {
		t.Fatalf("non-separator path = %q, want %q", got, want)
	}

	for _, value := range []string{
		" && file:ready",
		"file:ready && ",
		"file:first &&  && file:second",
	} {
		t.Run(value, func(t *testing.T) {
			if _, err := parseConfig([]string{value}); err == nil {
				t.Fatal("parseConfig unexpectedly accepted an empty compound member")
			}
		})
	}
}

func TestParseConfigAcceptsRepeatableFileConditions(t *testing.T) {
	config, err := parseConfig([]string{
		"file:relative/path:with-colon",
		"--or",
		"duration:250ms",
		"--or",
		"file:/tmp/ready",
		"--or=file::",
	})
	if err != nil {
		t.Fatalf("parseConfig returned error: %v", err)
	}
	if config.delay == nil || *config.delay != 250*time.Millisecond {
		t.Fatalf("delay = %v, want 250ms", config.delay)
	}
	if got, want := config.files, []string{"relative/path:with-colon", "/tmp/ready", ":"}; !equalStrings(got, want) {
		t.Fatalf("files = %q, want %q", got, want)
	}
}

func TestParseConfigRejectsEmptyFilePath(t *testing.T) {
	for _, arg := range []string{"file:"} {
		t.Run(arg, func(t *testing.T) {
			if _, err := parseConfig([]string{arg}); err == nil {
				t.Fatal("parseConfig unexpectedly succeeded")
			}
		})
	}
}

func TestParseConfigAcceptsFileOnlyAndMixedConditions(t *testing.T) {
	for _, args := range [][]string{
		{"file:ready"},
		{"file:ready", "--or=signal:USR1"},
	} {
		if _, err := parseConfig(args); err != nil {
			t.Fatalf("parseConfig(%q) returned error: %v", args, err)
		}
	}
}

func TestParseConfigPreservesDuplicateFileConditions(t *testing.T) {
	config, err := parseConfig([]string{"file:ready", "--or=file:ready"})
	if err != nil {
		t.Fatalf("parseConfig returned error: %v", err)
	}
	if got, want := config.files, []string{"ready", "ready"}; !equalStrings(got, want) {
		t.Fatalf("files = %q, want %q", got, want)
	}
}

func TestParseConfigAcceptsEventsFDForms(t *testing.T) {
	for _, args := range [][]string{
		{"--events-fd", "9", "duration:0s"},
		{"duration:0s", "--events-fd=9"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			config, err := parseConfig(args)
			if err != nil {
				t.Fatalf("parseConfig returned error: %v", err)
			}
			if config.eventsFD == nil || *config.eventsFD != 9 {
				t.Fatalf("eventsFD = %#v, want pointer to 9", config.eventsFD)
			}
		})
	}
}

func TestParseConfigRejectsInvalidEventsFD(t *testing.T) {
	for _, args := range [][]string{
		{"--events-fd", "duration:0s"},
		{"--events-fd=", "duration:0s"},
		{"--events-fd", "abc", "duration:0s"},
		{"--events-fd", "2", "duration:0s"},
		{"--events-fd=2", "duration:0s"},
		{"--events-fd", "9", "--events-fd", "10", "duration:0s"},
		{"duration:0s", "--events-fd=9", "--events-fd=10"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			if _, err := parseConfig(args); err == nil {
				t.Fatal("parseConfig unexpectedly accepted invalid --events-fd")
			}
		})
	}
}
