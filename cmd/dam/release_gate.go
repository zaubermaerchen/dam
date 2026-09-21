package main

// This file commits the runtime CLOSED-to-OPEN transition after the
// condition package has selected a root alternative.

import (
	"sync"

	"github.com/zaubermaerchen/dam/internal/condition"
	"github.com/zaubermaerchen/dam/internal/events"
)

type releaseGate struct {
	engine *condition.Engine
	events *events.Sink

	mu        sync.Mutex
	opened    bool
	completed bool
}

func newReleaseGate(engine *condition.Engine, sink *events.Sink) *releaseGate {
	return &releaseGate{engine: engine, events: sink}
}

func (gate *releaseGate) selected() <-chan struct{} {
	if gate == nil || gate.engine == nil {
		return nil
	}
	return gate.engine.Satisfied()
}

// commitOpen preserves the externally visible event ordering: selection is
// observed first, then the runtime marks OPEN, then stream-open is observed.
func (gate *releaseGate) commitOpen() error {
	if gate == nil {
		return nil
	}
	gate.mu.Lock()
	defer gate.mu.Unlock()
	if gate.completed || gate.opened {
		return nil
	}
	if gate.events != nil {
		gate.events.EmitReleaseSelected()
	}
	gate.opened = true
	if gate.events != nil {
		gate.events.EmitStreamOpen()
	}
	return nil
}

func (gate *releaseGate) completeEmpty() error {
	if gate == nil {
		return nil
	}
	// Engine owns the selection mutex, so this arbitration has no check-to-close
	// gap in which an already-selected root could be suppressed by empty EOF.
	if gate.engine != nil {
		selected, err := gate.engine.CompleteEmpty()
		if err != nil {
			return err
		}
		if selected {
			return gate.commitOpen()
		}
	}
	gate.mu.Lock()
	if gate.completed || gate.opened {
		gate.mu.Unlock()
		return nil
	}
	gate.completed = true
	gate.mu.Unlock()
	return nil
}

func (gate *releaseGate) close() {
	if gate != nil && gate.engine != nil {
		gate.engine.Close()
	}
}
