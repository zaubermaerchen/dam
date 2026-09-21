package main

// This file adapts the validated internal/cli plan to dam's private runtime
// condition model without exposing coordinator or monitor implementation types.

import (
	"slices"
	"time"

	"github.com/zaubermaerchen/dam/internal/cli"
)

const preReleaseBufferSize = 64 * 1024

type runConfig struct {
	delay    *time.Duration
	deadline *time.Time
	signals  []string
	files    []string
	groups   []releaseGroup
	eventsFD *int

	bufferSize        int
	immediateDuration bool
}

func (config runConfig) releaseGroups() []releaseGroup {
	return slices.Clone(config.groups)
}

func parseConfigAt(args []string, location *time.Location) (runConfig, error) {
	plan, err := cli.ParseAt(args, location)
	if err != nil {
		return runConfig{}, err
	}
	return runConfigFromPlan(plan), nil
}

func runConfigFromPlan(plan cli.Plan) runConfig {
	config := runConfig{
		bufferSize:        plan.BufferSize,
		immediateDuration: plan.ImmediateDuration,
	}
	if plan.EventsFD != nil {
		fd := *plan.EventsFD
		config.eventsFD = &fd
	}
	for _, group := range plan.Groups {
		runtimeGroup := releaseGroup{members: make([]releaseCondition, 0, len(group.Members))}
		for _, condition := range group.Members {
			runtimeCondition := releaseCondition{
				kind:     condition.Kind,
				source:   condition.Source,
				duration: condition.Duration,
				deadline: condition.Deadline,
			}
			runtimeGroup.members = append(runtimeGroup.members, runtimeCondition)
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
	return config
}

type releaseCondition struct {
	kind     string
	source   string
	duration time.Duration
	deadline time.Time
}

type releaseGroup struct {
	members []releaseCondition
}

func newDurationReleaseCondition(value time.Duration) releaseCondition {
	return releaseCondition{kind: "duration", source: value.String(), duration: value}
}

func newDatetimeReleaseCondition(value time.Time) releaseCondition {
	return releaseCondition{kind: "datetime", source: value.UTC().Format(time.RFC3339Nano), deadline: value}
}

func releaseChannelReady(release <-chan struct{}) bool {
	if release == nil {
		return false
	}
	select {
	case <-release:
		return true
	default:
		return false
	}
}
