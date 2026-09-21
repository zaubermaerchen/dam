package main

// This file adapts the validated internal/cli plan to the condition engine;
// command-line parsing remains outside the condition package.

import (
	"slices"
	"time"

	"github.com/zaubermaerchen/dam/internal/cli"
	conditionpkg "github.com/zaubermaerchen/dam/internal/condition"
)

const preReleaseBufferSize = 64 * 1024

type runConfig struct {
	conditionGroups []conditionpkg.Group
	eventsFD        *int

	bufferSize int
}

func (config runConfig) conditionPlan() []conditionpkg.Group {
	return slices.Clone(config.conditionGroups)
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
		bufferSize: plan.BufferSize,
	}
	if plan.EventsFD != nil {
		fd := *plan.EventsFD
		config.eventsFD = &fd
	}
	for _, group := range plan.Groups {
		conditionGroup := conditionpkg.Group{Members: make([]conditionpkg.Condition, 0, len(group.Members))}
		for _, condition := range group.Members {
			conditionGroup.Members = append(conditionGroup.Members, conditionpkg.Condition{
				Kind: condition.Kind, Source: condition.Source, Duration: condition.Duration, Deadline: condition.Deadline,
			})
		}
		config.conditionGroups = append(config.conditionGroups, conditionGroup)
	}
	return config
}
