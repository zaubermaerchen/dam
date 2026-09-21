package main

import (
	"testing"
	"time"

	"github.com/zaubermaerchen/dam/internal/condition"
)

func TestReleaseGateCommitsOnlyAfterConditionSelection(t *testing.T) {
	engine, err := condition.New([]condition.Group{{Members: []condition.Condition{{Kind: "duration", Duration: 0}}}}, condition.Options{})
	if err != nil {
		t.Fatal(err)
	}
	gate := newReleaseGate(engine, nil)
	if err := engine.Start(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-gate.selected():
	case <-time.After(time.Second):
		t.Fatal("condition root was not selected")
	}
	if gate.opened {
		t.Fatal("gate opened before the runtime commit")
	}
	if err := gate.commitOpen(); err != nil {
		t.Fatal(err)
	}
	if !gate.opened {
		t.Fatal("runtime gate did not commit OPEN")
	}
	gate.close()
}

func TestReleaseGateEmptyCompletionClosesConditionEngine(t *testing.T) {
	engine, err := condition.New([]condition.Group{{Members: []condition.Condition{{Kind: "duration", Duration: time.Hour}}}}, condition.Options{})
	if err != nil {
		t.Fatal(err)
	}
	gate := newReleaseGate(engine, nil)
	if err := engine.Start(); err != nil {
		t.Fatal(err)
	}
	if err := gate.completeEmpty(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-engine.Failures():
		t.Fatal("empty completion reported a monitor failure")
	default:
	}
	gate.close()
}

func TestReleaseGateSelectedRootWinsEmptyCompletion(t *testing.T) {
	engine, err := condition.New([]condition.Group{{Members: []condition.Condition{{Kind: "duration", Duration: 0}}}}, condition.Options{})
	if err != nil {
		t.Fatal(err)
	}
	gate := newReleaseGate(engine, nil)
	if err := engine.Start(); err != nil {
		t.Fatal(err)
	}
	if err := gate.completeEmpty(); err != nil {
		t.Fatal(err)
	}
	if !gate.opened {
		t.Fatal("selected root was suppressed by empty completion")
	}
	if gate.completed {
		t.Fatal("selected root was marked as empty completion")
	}
	gate.close()
}
