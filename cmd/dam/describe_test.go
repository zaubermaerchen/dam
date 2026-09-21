package main

// This file verifies dam's machine-readable static self-description.

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/zaubermaerchen/dam/internal/condition"
)

func TestRunDescribeWritesCompactJSONWithoutStartingRuntime(t *testing.T) {
	var output, diagnostics bytes.Buffer
	input := describePanicReader{}
	clock := runtimeClock{
		now: func() time.Time {
			t.Fatal("--describe called the runtime clock")
			return time.Time{}
		},
		location: time.UTC,
		newTimer: func(time.Duration) (<-chan time.Time, func()) {
			t.Fatal("--describe created a condition timer")
			return nil, nil
		},
	}

	if status := runWithClock([]string{"--describe"}, input, &output, &diagnostics, clock); status != 0 {
		t.Fatalf("runWithClock status = %d, diagnostics = %q", status, diagnostics.String())
	}
	if diagnostics.Len() != 0 {
		t.Fatalf("successful --describe wrote diagnostics: %q", diagnostics.String())
	}
	if got := strings.Count(output.String(), "\n"); got != 1 {
		t.Fatalf("--describe output newline count = %d, want 1: %q", got, output.String())
	}

	raw := bytes.TrimSuffix(output.Bytes(), []byte{'\n'})
	var compact bytes.Buffer
	if err := json.Compact(&compact, raw); err != nil {
		t.Fatalf("decode --describe JSON: %v; output = %q", err, output.String())
	}
	if !bytes.Equal(compact.Bytes(), raw) {
		t.Fatalf("--describe output is not compact JSON: %q", output.String())
	}
	decoder := json.NewDecoder(bytes.NewReader(output.Bytes()))
	var document map[string]json.RawMessage
	if err := decoder.Decode(&document); err != nil {
		t.Fatalf("decode --describe output: %v; output = %q", err, output.String())
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		t.Fatalf("--describe output has trailing data: %v; output = %q", err, output.String())
	}

	wantFields := []string{"schema_version", "name", "version", "cli_schema", "stream_semantics", "state_machine", "side_effects"}
	if len(document) != len(wantFields) {
		t.Fatalf("top-level field count = %d, want %d: %#v", len(document), len(wantFields), document)
	}
	for _, field := range wantFields {
		if _, ok := document[field]; !ok {
			t.Errorf("--describe output missing top-level field %q", field)
		}
	}
	var schemaVersion int
	if err := json.Unmarshal(document["schema_version"], &schemaVersion); err != nil {
		t.Fatalf("schema_version: %v", err)
	}
	if schemaVersion != 1 {
		t.Fatalf("schema_version = %d, want 1", schemaVersion)
	}
	var name string
	if err := json.Unmarshal(document["name"], &name); err != nil {
		t.Fatalf("name: %v", err)
	}
	if name != "dam" {
		t.Fatalf("name = %q, want dam", name)
	}
	var sideEffects []json.RawMessage
	if err := json.Unmarshal(document["side_effects"], &sideEffects); err != nil {
		t.Fatalf("side_effects: %v", err)
	}
	if len(sideEffects) != 0 {
		t.Fatalf("side_effects = %#v, want empty", sideEffects)
	}
}

func TestRunDescribeHelpHasPriority(t *testing.T) {
	var output, diagnostics bytes.Buffer
	if status := run([]string{"--describe", "--help"}, describePanicReader{}, &output, &diagnostics); status != 0 {
		t.Fatalf("run status = %d, diagnostics = %q", status, diagnostics.String())
	}
	if got, want := output.String(), helpText; got != want {
		t.Fatalf("output = %q, want help output", got)
	}
	if diagnostics.Len() != 0 {
		t.Fatalf("help wrote diagnostics: %q", diagnostics.String())
	}
}

func TestRunDescribeRejectsCombinations(t *testing.T) {
	for _, args := range [][]string{
		{"--describe", "duration:1s"},
		{"duration:1s", "--describe"},
		{"--describe", "--version"},
		{"--describe", "--or", "duration:1s"},
		{"--describe", "--buffer-size", "1K"},
		{"--describe", "--describe"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			var output, diagnostics bytes.Buffer
			if status := run(args, describePanicReader{}, &output, &diagnostics); status == 0 {
				t.Fatal("--describe combination unexpectedly succeeded")
			}
			if output.Len() != 0 {
				t.Fatalf("output = %q, want empty", output.String())
			}
			if !strings.Contains(diagnostics.String(), "--describe") {
				t.Fatalf("diagnostics = %q, want --describe error", diagnostics.String())
			}
		})
	}
}

func TestRunDescribeReportsOutputErrors(t *testing.T) {
	var diagnostics bytes.Buffer
	if status := run([]string{"--describe"}, describePanicReader{}, errorWriter{err: errors.New("description output unavailable")}, &diagnostics); status == 0 {
		t.Fatal("description output error unexpectedly succeeded")
	}
	if !strings.Contains(diagnostics.String(), "description output unavailable") {
		t.Fatalf("diagnostics = %q, want output error", diagnostics.String())
	}
}

func TestDescriptionMetadataMatchesDamContract(t *testing.T) {
	description := newDescription()

	if description.SchemaVersion != 1 || description.Name != "dam" || description.Version != version {
		t.Fatalf("description identity = %#v, want schema 1/dam/%q", description, version)
	}
	if len(description.CLISchema.Options) != 5 {
		t.Fatalf("option count = %d, want 5", len(description.CLISchema.Options))
	}
	wantTypes := map[string]string{
		"--describe":    "boolean",
		"--help":        "boolean",
		"--version":     "boolean",
		"--or":          "condition",
		"--buffer-size": "size",
	}
	seen := make(map[string]bool, len(description.CLISchema.Options))
	for _, option := range description.CLISchema.Options {
		if seen[option.Name] {
			t.Fatalf("duplicate option %q", option.Name)
		}
		seen[option.Name] = true
		if want, ok := wantTypes[option.Name]; !ok || option.Type != want {
			t.Errorf("option %s type = %q, want %q", option.Name, option.Type, want)
		}
	}
	for _, option := range description.CLISchema.Options {
		switch option.Name {
		case "--help", "--or", "--buffer-size":
			if !option.Repeatable {
				t.Errorf("option %s repeatable = false, want true", option.Name)
			}
		case "--describe", "--version":
			if option.Repeatable {
				t.Errorf("option %s repeatable = true, want false", option.Name)
			}
		}
	}
	for name := range wantTypes {
		if !seen[name] {
			t.Errorf("missing option %q", name)
		}
	}

	if len(description.CLISchema.ConditionForms) != 4 {
		t.Fatalf("condition form count = %d, want 4", len(description.CLISchema.ConditionForms))
	}
	if got := description.CLISchema.ConditionForms[0].Syntax; len(got) != 4 || got[0] != "signal:USR1" || got[1] != "signal:SIGUSR1" || got[2] != "signal:USR2" || got[3] != "signal:SIGUSR2" {
		t.Fatalf("signal syntax = %#v", got)
	}
	if description.CLISchema.ConditionForms[0].Supported == nil || *description.CLISchema.ConditionForms[0].Supported != condition.SignalSupported() {
		t.Fatalf("signal supported = %v, want capability %v", description.CLISchema.ConditionForms[0].Supported, condition.SignalSupported())
	}
	if got := description.CLISchema.ConditionForms[1].Syntax; len(got) != 1 || got[0] != "file:<path>" {
		t.Fatalf("file syntax = %#v", got)
	}
	if got := description.CLISchema.ConditionForms[2].Syntax; len(got) != 1 || got[0] != "duration:<value>" {
		t.Fatalf("duration syntax = %#v", got)
	}
	if got := description.CLISchema.ConditionForms[3].Syntax; len(got) != 5 {
		t.Fatalf("datetime syntax = %#v, want 5 forms", got)
	}

	if len(description.CLISchema.Arguments) != 1 {
		t.Fatalf("argument count = %d, want 1", len(description.CLISchema.Arguments))
	}
	argument := description.CLISchema.Arguments[0]
	if argument.Name != "CONDITION" || argument.Type != "condition" || !argument.Required || argument.Repeatable {
		t.Fatalf("condition argument = %#v, want required non-repeatable CONDITION", argument)
	}
	if argument.AndSeparator != " && " || argument.OrOption != "--or" || len(argument.OrForms) != 2 || argument.OrForms[0] != "--or CONDITION" || argument.OrForms[1] != "--or=CONDITION" {
		t.Fatalf("condition composition = %#v", argument)
	}
	for _, want := range []string{"exact literal", "not trimmed", "not escapable", "latched"} {
		if !strings.Contains(strings.ToLower(argument.AndSemantics), strings.ToLower(want)) {
			t.Errorf("AND semantics = %q, want %q", argument.AndSemantics, want)
		}
	}
	for _, want := range []string{"--or CONDITION", "--or=CONDITION"} {
		if !strings.Contains(argument.OrSemantics, want) {
			t.Errorf("OR semantics = %q, want %q", argument.OrSemantics, want)
		}
	}

	for _, test := range []struct {
		name   string
		stream streamInterfaceDescription
		role   string
	}{
		{name: "stdin", stream: description.StreamSemantics.Stdin, role: "input"},
		{name: "stdout", stream: description.StreamSemantics.Stdout, role: "passthrough"},
		{name: "stderr", stream: description.StreamSemantics.Stderr, role: "diagnostics"},
	} {
		if test.stream.Role != test.role || strings.TrimSpace(test.stream.Description) == "" {
			t.Fatalf("stream interface %s = %#v, want role %q and description", test.name, test.stream, test.role)
		}
	}
	streamText := strings.ToLower(description.StreamSemantics.Stdin.Description + " " + description.StreamSemantics.Stdout.Description)
	for _, want := range []string{"bounded", "backpressure", "eof"} {
		if !strings.Contains(streamText, want) {
			t.Errorf("stream semantics = %q, want %q", streamText, want)
		}
	}

	if description.StateMachine.InitialState != "closed" || !equalDescriptionStrings(description.StateMachine.States, []string{"closed", "closed-buffered-eof", "closed-buffered-error", "open", "empty-eof", "eof", "error"}) {
		t.Fatalf("state machine initial/states = %#v", description.StateMachine)
	}
	if !equalDescriptionStrings(description.StateMachine.Events, []string{"release-condition", "empty-eof", "buffered-eof", "buffered-error", "buffered-eof-complete", "buffered-error-complete", "eof", "error"}) {
		t.Fatalf("state machine events = %#v", description.StateMachine.Events)
	}
	wantTransitions := []stateTransition{
		{From: "closed", Event: "release-condition", To: "open"},
		{From: "closed", Event: "empty-eof", To: "empty-eof"},
		{From: "closed", Event: "buffered-eof", To: "closed-buffered-eof"},
		{From: "closed", Event: "buffered-error", To: "closed-buffered-error"},
		{From: "closed", Event: "error", To: "error"},
		{From: "closed-buffered-eof", Event: "release-condition", To: "open"},
		{From: "closed-buffered-error", Event: "release-condition", To: "open"},
		{From: "open", Event: "buffered-eof-complete", To: "eof", Automatic: true},
		{From: "open", Event: "buffered-error-complete", To: "error", Automatic: true},
		{From: "open", Event: "eof", To: "eof"},
		{From: "open", Event: "error", To: "error"},
	}
	if len(description.StateMachine.Transitions) != len(wantTransitions) {
		t.Fatalf("transition count = %d, want %d", len(description.StateMachine.Transitions), len(wantTransitions))
	}
	for i, want := range wantTransitions {
		if description.StateMachine.Transitions[i] != want {
			t.Errorf("transition %d = %#v, want %#v", i, description.StateMachine.Transitions[i], want)
		}
	}
	if len(description.SideEffects) != 0 {
		t.Fatalf("side effects = %#v, want empty", description.SideEffects)
	}
}

func TestDescriptionDocumentsBufferSizeGrammarAndPlacement(t *testing.T) {
	document := marshalDescriptionDocument(t)
	options := rawDescriptionOptions(t, document)
	bufferSize := options["--buffer-size"]
	if bufferSize == nil {
		t.Fatal("description is missing --buffer-size")
	}

	if got := rawDescriptionStrings(t, bufferSize, "forms"); !equalDescriptionStrings(got, []string{"--buffer-size SIZE", "--buffer-size=SIZE"}) {
		t.Fatalf("buffer-size forms = %#v, want separated and equals forms", got)
	}
	for field, want := range map[string]string{
		"value_grammar":    "positive decimal integer bytes with optional binary K/k, M/m, or G/g suffix",
		"placement":        "before, between, or after CONDITION and --or CONDITION",
		"repeat_semantics": "last value wins",
	} {
		if got := rawDescriptionString(t, bufferSize, field); !strings.Contains(strings.ToLower(got), strings.ToLower(want)) {
			t.Errorf("buffer-size %s = %q, want text containing %q", field, got, want)
		}
	}
}

func TestDescriptionDocumentsDurationStartAndZeroRelease(t *testing.T) {
	document := marshalDescriptionDocument(t)
	forms := rawDescriptionConditionForms(t, document)
	duration := forms["duration"]
	if duration == nil {
		t.Fatal("description is missing duration condition form")
	}
	if got := rawDescriptionString(t, duration, "trigger"); got != "first-non-empty-read-completion" {
		t.Fatalf("duration trigger = %q, want first-non-empty-read-completion", got)
	}
	if got := rawDescriptionString(t, duration, "zero_semantics"); got != "immediate-condition-satisfaction" {
		t.Fatalf("duration zero semantics = %q, want immediate-condition-satisfaction", got)
	}
}

func TestDescriptionDocumentsProcessLocalTimezoneAtStartup(t *testing.T) {
	document := marshalDescriptionDocument(t)
	forms := rawDescriptionConditionForms(t, document)
	datetime := forms["datetime"]
	if datetime == nil {
		t.Fatal("description is missing datetime condition form")
	}
	if got, want := rawDescriptionString(t, datetime, "timezone_less_timezone"), "process-local-timezone-at-startup"; got != want {
		t.Fatalf("timezone-less timezone = %q, want %q", got, want)
	}
}

func TestDescriptionStateModelSeparatesEmptyAndBufferedTerminalPaths(t *testing.T) {
	description := newDescription()
	wantStates := []string{
		"closed",
		"closed-buffered-eof",
		"closed-buffered-error",
		"open",
		"empty-eof",
		"eof",
		"error",
	}
	if !equalDescriptionStrings(description.StateMachine.States, wantStates) {
		t.Fatalf("states = %#v, want %#v", description.StateMachine.States, wantStates)
	}
	wantEvents := []string{"release-condition", "empty-eof", "buffered-eof", "buffered-error", "buffered-eof-complete", "buffered-error-complete", "eof", "error"}
	if !equalDescriptionStrings(description.StateMachine.Events, wantEvents) {
		t.Fatalf("events = %#v, want %#v", description.StateMachine.Events, wantEvents)
	}
	wantTransitions := []stateTransition{
		{From: "closed", Event: "release-condition", To: "open"},
		{From: "closed", Event: "empty-eof", To: "empty-eof"},
		{From: "closed", Event: "buffered-eof", To: "closed-buffered-eof"},
		{From: "closed", Event: "buffered-error", To: "closed-buffered-error"},
		{From: "closed", Event: "error", To: "error"},
		{From: "closed-buffered-eof", Event: "release-condition", To: "open"},
		{From: "closed-buffered-error", Event: "release-condition", To: "open"},
		{From: "open", Event: "buffered-eof-complete", To: "eof", Automatic: true},
		{From: "open", Event: "buffered-error-complete", To: "error", Automatic: true},
		{From: "open", Event: "eof", To: "eof"},
		{From: "open", Event: "error", To: "error"},
	}
	if len(description.StateMachine.Transitions) != len(wantTransitions) {
		t.Fatalf("transition count = %d, want %d: %#v", len(description.StateMachine.Transitions), len(wantTransitions), description.StateMachine.Transitions)
	}
	for index, want := range wantTransitions {
		if got := description.StateMachine.Transitions[index]; got != want {
			t.Errorf("transition %d = %#v, want %#v", index, got, want)
		}
	}
	document := marshalDescriptionDocument(t)
	var state map[string]json.RawMessage
	if err := json.Unmarshal(document["state_machine"], &state); err != nil {
		t.Fatalf("decode state machine: %v", err)
	}
	semantics := rawDescriptionString(t, state, "terminal_semantics")
	for _, want := range []string{"commits open before the first stdout write", "automatic", "no additional EOF", "buffered-eof-complete", "buffered-error-complete"} {
		if !strings.Contains(strings.ToLower(semantics), strings.ToLower(want)) {
			t.Errorf("terminal semantics = %q, want text containing %q", semantics, want)
		}
	}
}

func TestDescriptionMarksBufferedTerminalCompletionsAutomatic(t *testing.T) {
	document := marshalDescriptionDocument(t)
	var state struct {
		Transitions []map[string]json.RawMessage `json:"transitions"`
	}
	if err := json.Unmarshal(document["state_machine"], &state); err != nil {
		t.Fatalf("decode state machine: %v", err)
	}
	wantAutomatic := map[string]bool{
		"buffered-eof-complete":   true,
		"buffered-error-complete": true,
	}
	for _, transition := range state.Transitions {
		event := rawDescriptionString(t, transition, "event")
		rawAutomatic, markedAutomatic := transition["automatic"]
		if want := wantAutomatic[event]; want {
			if !markedAutomatic {
				t.Errorf("transition %q is missing automatic=true", event)
				continue
			}
			var automatic bool
			if err := json.Unmarshal(rawAutomatic, &automatic); err != nil {
				t.Errorf("transition %q automatic: %v", event, err)
			} else if !automatic {
				t.Errorf("transition %q automatic = false, want true", event)
			}
		} else if markedAutomatic {
			t.Errorf("transition %q unexpectedly has automatic=%s", event, rawAutomatic)
		}
	}
}

func marshalDescriptionDocument(t *testing.T) map[string]json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(newDescription())
	if err != nil {
		t.Fatalf("marshal description: %v", err)
	}
	var document map[string]json.RawMessage
	if err := json.Unmarshal(raw, &document); err != nil {
		t.Fatalf("unmarshal description: %v", err)
	}
	return document
}

func rawDescriptionOptions(t *testing.T, document map[string]json.RawMessage) map[string]map[string]json.RawMessage {
	t.Helper()
	var cli struct {
		Options []map[string]json.RawMessage `json:"options"`
	}
	if err := json.Unmarshal(document["cli_schema"], &cli); err != nil {
		t.Fatalf("decode CLI schema: %v", err)
	}
	options := make(map[string]map[string]json.RawMessage, len(cli.Options))
	for _, option := range cli.Options {
		name := rawDescriptionString(t, option, "name")
		options[name] = option
	}
	return options
}

func rawDescriptionConditionForms(t *testing.T, document map[string]json.RawMessage) map[string]map[string]json.RawMessage {
	t.Helper()
	var cli struct {
		ConditionForms []map[string]json.RawMessage `json:"condition_forms"`
	}
	if err := json.Unmarshal(document["cli_schema"], &cli); err != nil {
		t.Fatalf("decode CLI schema: %v", err)
	}
	forms := make(map[string]map[string]json.RawMessage, len(cli.ConditionForms))
	for _, form := range cli.ConditionForms {
		name := rawDescriptionString(t, form, "name")
		forms[name] = form
	}
	return forms
}

func rawDescriptionString(t *testing.T, fields map[string]json.RawMessage, name string) string {
	t.Helper()
	raw, ok := fields[name]
	if !ok {
		t.Fatalf("description is missing %q", name)
	}
	var value string
	if err := json.Unmarshal(raw, &value); err != nil {
		t.Fatalf("decode description field %q: %v", name, err)
	}
	return value
}

func rawDescriptionStrings(t *testing.T, fields map[string]json.RawMessage, name string) []string {
	t.Helper()
	raw, ok := fields[name]
	if !ok {
		t.Fatalf("description is missing %q", name)
	}
	var value []string
	if err := json.Unmarshal(raw, &value); err != nil {
		t.Fatalf("decode description field %q: %v", name, err)
	}
	return value
}

func equalDescriptionStrings(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

type describePanicReader struct{}

func (describePanicReader) Read([]byte) (int, error) {
	panic("--describe must not read stdin")
}
