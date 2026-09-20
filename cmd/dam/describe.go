package main

// This file defines and renders dam's machine-readable static CLI description.

import (
	"encoding/json"
	"io"
)

type description struct {
	SchemaVersion   int               `json:"schema_version"`
	Name            string            `json:"name"`
	Version         string            `json:"version"`
	CLISchema       cliDescription    `json:"cli_schema"`
	StreamSemantics streamDescription `json:"stream_semantics"`
	StateMachine    stateDescription  `json:"state_machine"`
	SideEffects     []sideEffect      `json:"side_effects"`
}

type cliDescription struct {
	Options        []cliOptionDescription     `json:"options"`
	ConditionForms []conditionFormDescription `json:"condition_forms"`
	Arguments      []cliArgumentDescription   `json:"arguments"`
}

type cliOptionDescription struct {
	Name            string   `json:"name"`
	Type            string   `json:"type"`
	Required        bool     `json:"required"`
	Repeatable      bool     `json:"repeatable"`
	Aliases         []string `json:"aliases,omitempty"`
	Conflicts       []string `json:"conflicts,omitempty"`
	Standalone      bool     `json:"standalone,omitempty"`
	Priority        string   `json:"priority,omitempty"`
	Forms           []string `json:"forms,omitempty"`
	ValueGrammar    string   `json:"value_grammar,omitempty"`
	Placement       string   `json:"placement,omitempty"`
	RepeatSemantics string   `json:"repeat_semantics,omitempty"`
}

type cliArgumentDescription struct {
	Name         string   `json:"name"`
	Type         string   `json:"type"`
	Required     bool     `json:"required"`
	Repeatable   bool     `json:"repeatable"`
	Conflicts    []string `json:"conflicts,omitempty"`
	AndSeparator string   `json:"and_separator"`
	AndSemantics string   `json:"and_semantics"`
	OrOption     string   `json:"or_option"`
	OrSemantics  string   `json:"or_semantics"`
	OrForms      []string `json:"or_forms"`
}

// conditionFormDescription is the small, static public vocabulary for
// positional condition values. Pointer fields omit constraints that do not
// apply to a particular condition form while preserving explicit false values.
type conditionFormDescription struct {
	Name                            string   `json:"name"`
	Syntax                          []string `json:"syntax"`
	Supported                       *bool    `json:"supported,omitempty"`
	NonEmpty                        *bool    `json:"non_empty,omitempty"`
	ValuePreserved                  *bool    `json:"value_preserved,omitempty"`
	Trigger                         string   `json:"trigger,omitempty"`
	ValueFormat                     string   `json:"value_format,omitempty"`
	NonNegative                     *bool    `json:"non_negative,omitempty"`
	ZeroAllowed                     *bool    `json:"zero_allowed,omitempty"`
	ZeroSemantics                   string   `json:"zero_semantics,omitempty"`
	TimezoneLessTimezone            string   `json:"timezone_less_timezone,omitempty"`
	ExplicitTimezoneRequiresSeconds *bool    `json:"explicit_timezone_requires_seconds,omitempty"`
	Fractional                      *bool    `json:"fractional,omitempty"`
	IANA                            *bool    `json:"iana,omitempty"`
	TimezoneLessDSTGap              string   `json:"timezone_less_dst_gap,omitempty"`
	TimezoneLessDSTOverlap          string   `json:"timezone_less_dst_overlap,omitempty"`
}

func descriptionBool(value bool) *bool {
	return &value
}

type streamDescription struct {
	Stdin  streamInterfaceDescription `json:"stdin"`
	Stdout streamInterfaceDescription `json:"stdout"`
	Stderr streamInterfaceDescription `json:"stderr"`
}

type streamInterfaceDescription struct {
	Role        string `json:"role"`
	Description string `json:"description"`
	Format      string `json:"format,omitempty"`
	Option      string `json:"option,omitempty"`
}

type stateDescription struct {
	InitialState      string            `json:"initial_state"`
	States            []string          `json:"states"`
	Events            []string          `json:"events"`
	Transitions       []stateTransition `json:"transitions"`
	TerminalSemantics string            `json:"terminal_semantics"`
}

type stateTransition struct {
	From      string `json:"from"`
	Event     string `json:"event"`
	To        string `json:"to"`
	Automatic bool   `json:"automatic,omitempty"`
}

type sideEffect struct{}

func describeRequested(args []string) bool {
	for _, arg := range args {
		if arg == "--describe" {
			return true
		}
	}
	return false
}

func newDescription() description {
	signalSupported := descriptionBool(signalReleaseSupported())
	return description{
		SchemaVersion: 1,
		Name:          "dam",
		Version:       version,
		CLISchema: cliDescription{
			Options: []cliOptionDescription{
				{
					Name:       "--describe",
					Type:       "boolean",
					Conflicts:  []string{"--version", "--or", "--buffer-size", "CONDITION"},
					Standalone: true,
				},
				{
					Name:       "--help",
					Type:       "boolean",
					Repeatable: true,
					Aliases:    []string{"-h"},
					Priority:   "highest",
				},
				{
					Name:       "--version",
					Type:       "boolean",
					Conflicts:  []string{"--describe", "--or", "--buffer-size", "CONDITION"},
					Standalone: true,
				},
				{
					Name:       "--or",
					Type:       "condition",
					Repeatable: true,
					Conflicts:  []string{"--describe", "--version"},
				},
				{
					Name:            "--buffer-size",
					Type:            "size",
					Repeatable:      true,
					Conflicts:       []string{"--describe", "--version"},
					Forms:           []string{"--buffer-size SIZE", "--buffer-size=SIZE"},
					ValueGrammar:    "positive decimal integer bytes with optional binary K/k, M/m, or G/g suffix (base 1024); zero, signs, decimals, and other suffixes are invalid",
					Placement:       "before, between, or after CONDITION and --or CONDITION",
					RepeatSemantics: "last value wins",
				},
			},
			ConditionForms: []conditionFormDescription{
				{
					Name:      "signal",
					Syntax:    []string{"signal:USR1", "signal:SIGUSR1", "signal:USR2", "signal:SIGUSR2"},
					Supported: signalSupported,
				},
				{
					Name:           "file",
					Syntax:         []string{"file:<path>"},
					NonEmpty:       descriptionBool(true),
					ValuePreserved: descriptionBool(true),
					Trigger:        "path-resolves-to-regular-file",
				},
				{
					Name:          "duration",
					Syntax:        []string{"duration:<value>"},
					Trigger:       "first-non-empty-read-completion",
					ValueFormat:   "go-duration",
					NonNegative:   descriptionBool(true),
					ZeroAllowed:   descriptionBool(true),
					ZeroSemantics: "immediate-condition-satisfaction",
				},
				{
					Name:                            "datetime",
					Syntax:                          []string{"datetime:YYYY-MM-DDTHH:MM", "datetime:YYYY-MM-DDTHH:MM:SS", "datetime:YYYY-MM-DDTHH:MM:SSZ", "datetime:YYYY-MM-DDTHH:MM:SS+HH:MM", "datetime:YYYY-MM-DDTHH:MM:SS-HH:MM"},
					TimezoneLessTimezone:            "process-local-timezone-at-startup",
					ExplicitTimezoneRequiresSeconds: descriptionBool(true),
					Fractional:                      descriptionBool(false),
					IANA:                            descriptionBool(false),
					TimezoneLessDSTGap:              "invalid",
					TimezoneLessDSTOverlap:          "earliest-absolute-instant",
				},
			},
			Arguments: []cliArgumentDescription{
				{
					Name:         "CONDITION",
					Type:         "condition",
					Required:     true,
					Conflicts:    []string{"--describe", "--version"},
					AndSeparator: " && ",
					AndSemantics: "Members are separated by the exact literal \" && \" separator; operands are not trimmed and the separator is not escapable. Every member must be satisfied and remains latched.",
					OrOption:     "--or",
					OrSemantics:  "Each --or CONDITION or --or=CONDITION starts an alternative AND group; dam opens when any group is satisfied.",
					OrForms:      []string{"--or CONDITION", "--or=CONDITION"},
				},
			},
		},
		StreamSemantics: streamDescription{
			Stdin: streamInterfaceDescription{
				Role:        "input",
				Description: "Primary input stream read from stdin. Before the gate opens, data is held in a bounded pre-release buffer; once that buffer is full, ordinary pipe backpressure limits further input. Empty stdin EOF exits successfully without waiting for a release condition, while data read before EOF remains held until release.",
			},
			Stdout: streamInterfaceDescription{
				Role:        "passthrough",
				Description: "Primary output stream containing the original input bytes byte-for-byte and in order. No stream data is written while the gate is closed; after it opens, the held data is written and later input is forwarded without another gate.",
			},
			Stderr: streamInterfaceDescription{
				Role:        "diagnostics",
				Description: "Human-readable diagnostics, argument errors, and I/O errors; it is separate from the passthrough data stream.",
			},
		},
		StateMachine: stateDescription{
			InitialState: "closed",
			States:       []string{"closed", "closed-buffered-eof", "closed-buffered-error", "open", "empty-eof", "eof", "error"},
			Events:       []string{"release-condition", "empty-eof", "buffered-eof", "buffered-error", "buffered-eof-complete", "buffered-error-complete", "eof", "error"},
			Transitions: []stateTransition{
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
			},
			TerminalSemantics: "release-condition commits open before the first stdout write; buffered EOF/error observed while closed completes through automatic buffered-eof-complete or buffered-error-complete after those held bytes drain, and those completion events require no additional EOF, error, or external event; eof/error describe terminal results first observed after open.",
		},
		SideEffects: []sideEffect{},
	}
}

func printDescription(out io.Writer) error {
	return json.NewEncoder(out).Encode(newDescription())
}
