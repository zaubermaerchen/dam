package cli

// Package cli parses dam's command-line conditions and runtime options.

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

const defaultBufferSize = 64 * 1024

// Plan is the validated command-line configuration consumed by the runtime.
// Groups preserve argument order and duplicate file members exactly as given.
type Plan struct {
	Groups            []Group
	BufferSize        int
	EventsFD          *int
	ImmediateDuration bool
}

// Group is one AND alternative in a command-line release plan.
type Group struct {
	Members []Condition
}

// Condition is a parsed release condition. Kind is one of duration, datetime,
// signal, or file; Source preserves the normalized signal or original path.
type Condition struct {
	Kind     string
	Source   string
	Duration time.Duration
	Deadline time.Time
}

// Parse parses arguments using the process local timezone.
func Parse(args []string) (Plan, error) {
	return ParseAt(args, time.Local)
}

// ParseAt parses arguments using location for timezone-less datetime values.
func ParseAt(args []string, location *time.Location) (Plan, error) {
	location = normalizeLocation(location)
	plan := Plan{BufferSize: defaultBufferSize}
	hasCondition := false
	pendingOR := false
	for index := 0; index < len(args); index++ {
		arg := args[index]
		switch {
		case arg == "--buffer-size":
			index++
			if index == len(args) {
				return Plan{}, fmt.Errorf("missing value for --buffer-size")
			}
			bufferSize, err := parseBufferSize(args[index])
			if err != nil {
				return Plan{}, err
			}
			plan.BufferSize = bufferSize
		case strings.HasPrefix(arg, "--buffer-size="):
			bufferSize, err := parseBufferSize(strings.TrimPrefix(arg, "--buffer-size="))
			if err != nil {
				return Plan{}, err
			}
			plan.BufferSize = bufferSize
		case arg == "--events-fd":
			if plan.EventsFD != nil {
				return Plan{}, fmt.Errorf("--events-fd may only be specified once")
			}
			index++
			if index == len(args) {
				return Plan{}, fmt.Errorf("missing value for --events-fd")
			}
			eventsFD, err := parseEventsFD(args[index])
			if err != nil {
				return Plan{}, err
			}
			plan.EventsFD = &eventsFD
		case strings.HasPrefix(arg, "--events-fd="):
			if plan.EventsFD != nil {
				return Plan{}, fmt.Errorf("--events-fd may only be specified once")
			}
			eventsFD, err := parseEventsFD(strings.TrimPrefix(arg, "--events-fd="))
			if err != nil {
				return Plan{}, err
			}
			plan.EventsFD = &eventsFD
		case arg == "--or":
			if !hasCondition {
				return Plan{}, fmt.Errorf("--or requires a preceding condition")
			}
			if pendingOR {
				return Plan{}, fmt.Errorf("--or requires a condition")
			}
			pendingOR = true
		case strings.HasPrefix(arg, "--or="):
			if !hasCondition {
				return Plan{}, fmt.Errorf("--or requires a preceding condition")
			}
			if pendingOR {
				return Plan{}, fmt.Errorf("--or requires a condition")
			}
			value := strings.TrimPrefix(arg, "--or=")
			if value == "" {
				return Plan{}, fmt.Errorf("--or requires a condition")
			}
			group, err := parseGroupAt(value, location)
			if err != nil {
				return Plan{}, err
			}
			plan.addGroup(group)
			pendingOR = false
		case strings.HasPrefix(arg, "--"):
			return Plan{}, fmt.Errorf("unknown option %q", arg)
		default:
			if hasCondition && !pendingOR {
				return Plan{}, fmt.Errorf("unexpected argument %q: use --or between conditions", arg)
			}
			group, err := parseGroupAt(arg, location)
			if err != nil {
				return Plan{}, err
			}
			plan.addGroup(group)
			hasCondition = true
			pendingOR = false
		}
	}
	if !hasCondition {
		return Plan{}, fmt.Errorf("usage: dam CONDITION [--or CONDITION]... [--buffer-size SIZE]")
	}
	if pendingOR {
		return Plan{}, fmt.Errorf("--or requires a condition")
	}
	return plan, nil
}

func (plan *Plan) addGroup(group Group) {
	plan.Groups = append(plan.Groups, group)
	for _, condition := range group.Members {
		if condition.Kind == "duration" && condition.Duration == 0 {
			plan.ImmediateDuration = true
		}
	}
}

func parseGroupAt(value string, location *time.Location) (Group, error) {
	parts := strings.Split(value, " && ")
	group := Group{Members: make([]Condition, 0, len(parts))}
	for _, part := range parts {
		condition, err := parseConditionAt(part, location)
		if err != nil {
			return Group{}, err
		}
		group.Members = append(group.Members, condition)
	}
	return group, nil
}

func parseConditionAt(value string, location *time.Location) (Condition, error) {
	location = normalizeLocation(location)
	typeName, source, ok := strings.Cut(value, ":")
	if !ok {
		return Condition{}, invalidReleaseCondition(value)
	}
	switch typeName {
	case "duration":
		duration, err := time.ParseDuration(source)
		if err != nil {
			return Condition{}, invalidReleaseCondition(value)
		}
		if duration < 0 {
			return Condition{}, fmt.Errorf("invalid release condition %q: duration must not be negative", value)
		}
		return Condition{Kind: "duration", Source: duration.String(), Duration: duration}, nil
	case "datetime":
		deadline, err := parseAbsoluteDeadline(source, location)
		if err != nil {
			return Condition{}, invalidReleaseCondition(value)
		}
		return Condition{Kind: "datetime", Source: deadline.UTC().Format(time.RFC3339Nano), Deadline: deadline}, nil
	case "signal":
		signal, err := parseSignalSource(value, source)
		if err != nil {
			return Condition{}, err
		}
		return Condition{Kind: "signal", Source: signal}, nil
	case "file":
		if source == "" {
			return Condition{}, fmt.Errorf("invalid release condition %q: file path must not be empty", value)
		}
		return Condition{Kind: "file", Source: source}, nil
	default:
		return Condition{}, invalidReleaseCondition(value)
	}
}

func parseSignalSource(value, source string) (string, error) {
	switch source {
	case "USR1", "SIGUSR1":
		return "SIGUSR1", nil
	case "USR2", "SIGUSR2":
		return "SIGUSR2", nil
	default:
		return "", invalidReleaseCondition(value)
	}
}

func invalidReleaseCondition(value string) error {
	return fmt.Errorf("invalid release condition %q: want duration:DURATION, datetime:YYYY-MM-DDTHH:MM[:SS], datetime:YYYY-MM-DDTHH:MM:SS[Z|+HH:MM|-HH:MM], signal:USR1, signal:SIGUSR1, signal:USR2, signal:SIGUSR2, or file:PATH", value)
}

func normalizeLocation(location *time.Location) *time.Location {
	if location != nil {
		return location
	}
	if time.Local != nil {
		return time.Local
	}
	return time.UTC
}

func parseAbsoluteDeadline(value string, location *time.Location) (time.Time, error) {
	location = normalizeLocation(location)
	if (len(value) == 20 && value[19] == 'Z') ||
		(len(value) == 25 && (value[19] == '+' || value[19] == '-')) {
		if len(value) == 25 {
			// time.Parse normalizes some out-of-range offset minutes, so
			// validate the textual RFC3339 offset before parsing it.
			if !allASCIIDigits(value[20:22]) || value[22] != ':' || !allASCIIDigits(value[23:25]) {
				return time.Time{}, fmt.Errorf("datetime timezone offset must use +HH:MM or -HH:MM")
			}
			if parseASCIIDigits(value[20:22]) > 23 || parseASCIIDigits(value[23:25]) > 59 {
				return time.Time{}, fmt.Errorf("datetime timezone offset is out of range")
			}
		}
		parsed, err := time.Parse(time.RFC3339, value)
		if err != nil {
			return time.Time{}, fmt.Errorf("datetime must use RFC3339 with seconds and timezone: %w", err)
		}
		if parsed.Year() < 1 || parsed.Year() > 9999 {
			return time.Time{}, fmt.Errorf("datetime year must be between 0001 and 9999")
		}
		return parsed, nil
	}
	if len(value) != len("2006-01-02T15:04") && len(value) != len("2006-01-02T15:04:05") {
		return time.Time{}, fmt.Errorf("absolute deadline must use YYYY-MM-DDTHH:MM[:SS]")
	}
	if value[4] != '-' || value[7] != '-' || value[10] != 'T' || value[13] != ':' {
		return time.Time{}, fmt.Errorf("absolute deadline must use YYYY-MM-DDTHH:MM[:SS]")
	}
	if len(value) == 19 && value[16] != ':' {
		return time.Time{}, fmt.Errorf("absolute deadline must use YYYY-MM-DDTHH:MM:SS")
	}
	if !allASCIIDigits(value[:4]) || !allASCIIDigits(value[5:7]) || !allASCIIDigits(value[8:10]) || !allASCIIDigits(value[11:13]) || !allASCIIDigits(value[14:16]) {
		return time.Time{}, fmt.Errorf("absolute deadline contains non-numeric fields")
	}
	year := parseASCIIDigits(value[:4])
	month := time.Month(parseASCIIDigits(value[5:7]))
	day := parseASCIIDigits(value[8:10])
	hour := parseASCIIDigits(value[11:13])
	minute := parseASCIIDigits(value[14:16])
	second := 0
	if len(value) == 19 {
		if !allASCIIDigits(value[17:19]) {
			return time.Time{}, fmt.Errorf("absolute deadline contains non-numeric fields")
		}
		second = parseASCIIDigits(value[17:19])
	}
	if year < 1 || year > 9999 {
		return time.Time{}, fmt.Errorf("absolute deadline year must be between 0001 and 9999")
	}

	parsed := time.Date(year, month, day, hour, minute, second, 0, location)
	if !sameLocalDateTime(parsed, year, month, day, hour, minute, second) {
		return time.Time{}, fmt.Errorf("absolute deadline is not a valid local datetime")
	}
	return earliestLocalInstant(parsed, year, month, day, hour, minute, second, location), nil
}

func allASCIIDigits(value string) bool {
	if value == "" {
		return false
	}
	for index := 0; index < len(value); index++ {
		if value[index] < '0' || value[index] > '9' {
			return false
		}
	}
	return true
}

func parseASCIIDigits(value string) int {
	result := 0
	for index := 0; index < len(value); index++ {
		result = result*10 + int(value[index]-'0')
	}
	return result
}

func sameLocalDateTime(value time.Time, year int, month time.Month, day, hour, minute, second int) bool {
	return value.Year() == year && value.Month() == month && value.Day() == day &&
		value.Hour() == hour && value.Minute() == minute && value.Second() == second && value.Nanosecond() == 0
}

func earliestLocalInstant(parsed time.Time, year int, month time.Month, day, hour, minute, second int, location *time.Location) time.Time {
	start, end := parsed.ZoneBounds()
	// Compare the full value instead of calling IsZero: a real TZif boundary at
	// 0001-01-01T00:00:00Z is a zero instant carrying location information.
	if start == (time.Time{}) && end == (time.Time{}) {
		// UTC and FixedZone locations have one unbounded interval, so the
		// already validated parse is the only possible occurrence. This also
		// avoids imposing TZif's int32 offset representation on FixedZone's
		// unrestricted FixedZone offset.
		return parsed
	}

	wall := time.Date(year, month, day, hour, minute, second, 0, time.UTC)
	best := parsed
	considerOffset := func(offset int) {
		const (
			minInt64              = -1 << 63
			maxInt64              = 1<<63 - 1
			unixToInternalSeconds = 62135596800
			maxTimeUnix           = maxInt64 - unixToInternalSeconds
		)
		wallUnix := wall.Unix()
		offsetSeconds := int64(offset)
		if offsetSeconds > 0 && wallUnix < minInt64+offsetSeconds ||
			offsetSeconds < 0 && wallUnix > maxInt64+offsetSeconds {
			return
		}
		candidateUnix := wallUnix - offsetSeconds
		if candidateUnix > maxTimeUnix {
			return
		}
		candidate := time.Unix(candidateUnix, 0).In(location)
		if sameLocalDateTime(candidate, year, month, day, hour, minute, second) && candidate.Before(best) {
			best = candidate
		}
	}

	if start != (time.Time{}) {
		_, offset := start.Add(-time.Nanosecond).Zone()
		considerOffset(offset)
	}
	if end != (time.Time{}) {
		_, offset := end.Zone()
		considerOffset(offset)
	}

	const (
		minTZifOffset = -1 << 31
		maxTZifOffset = 1<<31 - 1
	)
	lastCandidate := wall.Add(-time.Duration(minTZifOffset) * time.Second)
	for current := wall.Add(-time.Duration(maxTZifOffset) * time.Second).In(location); !current.After(lastCandidate); {
		_, offset := current.Zone()
		considerOffset(offset)

		_, end := current.ZoneBounds()
		if end == (time.Time{}) || end.After(lastCandidate) || !end.After(current) {
			break
		}
		current = end
	}
	return best
}

func parseBufferSize(value string) (int, error) {
	if value == "" {
		return 0, fmt.Errorf("invalid buffer size %q", value)
	}

	multiplier := uint64(1)
	digits := value
	switch value[len(value)-1] {
	case 'K', 'k':
		multiplier = 1 << 10
		digits = value[:len(value)-1]
	case 'M', 'm':
		multiplier = 1 << 20
		digits = value[:len(value)-1]
	case 'G', 'g':
		multiplier = 1 << 30
		digits = value[:len(value)-1]
	}
	if digits == "" {
		return 0, fmt.Errorf("invalid buffer size %q", value)
	}
	for index := 0; index < len(digits); index++ {
		if digits[index] < '0' || digits[index] > '9' {
			return 0, fmt.Errorf("invalid buffer size %q", value)
		}
	}

	bytes, err := strconv.ParseUint(digits, 10, 64)
	if err != nil || bytes == 0 {
		return 0, fmt.Errorf("invalid buffer size %q", value)
	}
	maxInt := uint64(^uint(0) >> 1)
	if bytes > maxInt/multiplier {
		return 0, fmt.Errorf("invalid buffer size %q", value)
	}
	return int(bytes * multiplier), nil
}

func parseEventsFD(value string) (int, error) {
	if value == "" {
		return 0, fmt.Errorf("invalid --events-fd value %q", value)
	}
	parsed, err := strconv.ParseUint(value, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid --events-fd value %q", value)
	}
	maxInt := uint64(^uint(0) >> 1)
	if parsed < 3 || parsed > maxInt {
		return 0, fmt.Errorf("invalid --events-fd value %q: want an integer >= 3", value)
	}
	return int(parsed), nil
}
