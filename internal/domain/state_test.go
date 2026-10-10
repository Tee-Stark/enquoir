package domain

import "testing"

func TestState_Transition(t *testing.T) {
	legal := map[State][]State{
		PROPOSED:   {AUTHORIZED, BLOCKED},
		AUTHORIZED: {IN_FLIGHT, BLOCKED},
		IN_FLIGHT:  {APPLIED, FAILED, UNKNOWN},
		UNKNOWN:    {APPLIED, FAILED, IN_FLIGHT},
		FAILED:     {AUTHORIZED, BLOCKED},
		APPLIED:    {},
		BLOCKED:    {},
	}

	allStates := []State{PROPOSED, AUTHORIZED, IN_FLIGHT, UNKNOWN, APPLIED, FAILED, BLOCKED}

	for from, allowed := range legal {
		allowedSet := make(map[State]bool, len(allowed))
		for _, to := range allowed {
			allowedSet[to] = true
		}

		for _, to := range allStates {
			err := from.Transition(to)
			wantOK := allowedSet[to]

			if wantOK && err != nil {
				t.Errorf("%s -> %s: expected legal transition, got error: %v", from, to, err)
			}
			if !wantOK && err == nil {
				t.Errorf("%s -> %s: expected illegal transition to be rejected, got nil error", from, to)
			}
		}
	}
}

func TestState_TerminalStatesHaveNoLegalTransitions(t *testing.T) {
	for _, terminal := range []State{APPLIED, BLOCKED} {
		if !terminal.IsTerminal() {
			t.Errorf("%s: expected IsTerminal() to be true", terminal)
		}
		for _, to := range []State{PROPOSED, AUTHORIZED, IN_FLIGHT, UNKNOWN, APPLIED, FAILED, BLOCKED} {
			if err := terminal.Transition(to); err == nil {
				t.Errorf("%s -> %s: terminal state must reject every transition", terminal, to)
			}
		}
	}
}

func TestState_FailedIsNotTerminal(t *testing.T) {
	// FAILED is a parked state the reconciler can still move out of
	// (e.g. back to IN_FLIGHT on retry); only APPLIED and BLOCKED end
	// an intent's life.
	if FAILED.IsTerminal() {
		t.Error("FAILED.IsTerminal() = true, want false")
	}
}
