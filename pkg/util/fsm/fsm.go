// Copyright (C) 2017 ScyllaDB

package fsm

import (
	"context"
	"sync"

	"github.com/pkg/errors"
)

// ErrEventRejected is the error returned when the state machine cannot process
// an event in the state that it is in.
var ErrEventRejected = errors.New("event rejected")

const (
	// Default represents the default state of the system.
	Default StateType = ""

	// NoOp represents a no-op event.
	NoOp EventType = "NoOp"
)

// StateType represents an extensible state type in the state machine.
type StateType string

// EventType represents an extensible event type in the state machine.
type EventType string

// EventContext represents the context to be passed to the action implementation.
type EventContext interface{}

// Action represents the action to be executed in a given state.
type Action func(ctx context.Context) (EventType, error)

// Events represents a mapping of events and states.
type Events map[EventType]StateType

// State binds a state with an action and a set of events it can handle.
type State struct {
	Action Action
	Events Events
}

type Hook func(ctx context.Context, currentState, nextState StateType, event EventType) error

// States represents a mapping of states and their implementations.
type States map[StateType]State

// StateMachine represents the state machine.
type StateMachine struct {
	// Current represents the current state.
	Current StateType

	// States holds the configuration of states and events handled by the state machine.
	States States

	// TransitionHook is called on every state transition.
	TransitionHook Hook

	mutex sync.Mutex
}

// getNextState returns the next state for the event given the machine's current
// state, or an error if the event can't be handled in the given state.
func (s *StateMachine) getNextState(event EventType) (StateType, error) {
	if state, ok := s.States[s.Current]; ok {
		if state.Events != nil {
			if next, ok := state.Events[event]; ok {
				return next, nil
			}
		}
	}
	return Default, ErrEventRejected
}

// Transition triggers current state action and sends event to the state machine.
func (s *StateMachine) Transition(ctx context.Context) error {
	s.mutex.Lock()
	defer s.mutex.Unlock()

	state := s.States[s.Current]
	nextEvent, err := state.Action(ctx)
	if err != nil {
		return err
	}
	if nextEvent == NoOp {
		return nil
	}

	return s.sendEvent(ctx, nextEvent)
}

func (s *StateMachine) sendEvent(ctx context.Context, event EventType) error {
	for {
		// Determine the next state for the event given the machine's current state.
		nextState, err := s.getNextState(event)
		if err != nil {
			return errors.Wrapf(ErrEventRejected, "rejected %s", err.Error())
		}

		// Identify the state definition for the next state.
		state, ok := s.States[nextState]
		if !ok || state.Action == nil {
			return errors.Wrapf(ErrEventRejected, "unknown state %q for event %q", nextState, event)
		}

		if s.TransitionHook != nil {
			if err := s.TransitionHook(ctx, s.Current, nextState, event); err != nil {
				return err
			}
		}
		// Transition over to the next state.
		s.Current = nextState

		// Execute the next state's action and loop over again if the event returned
		// is not a no-op.
		nextEvent, err := state.Action(ctx)
		if err != nil {
			return err
		}
		if nextEvent == NoOp {
			return nil
		}
		event = nextEvent
	}
}
