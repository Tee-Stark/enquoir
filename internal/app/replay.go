package app

import (
	"context"
	"fmt"
	"sort"
	"time"

	"enquoir/internal/domain"
)

// Replay step types.
const (
	StepSubmission = "submission" // an agent submitted the intent
	StepTransition = "transition" // the payment moved state (an event)
	StepRailCall   = "rail_call"  // Enquoir called Airwallex
)

// Actors: who acted in a step.
const (
	ActorAgent    = "agent"
	ActorPolicy   = "policy"
	ActorWorkflow = "workflow"
	ActorRail     = "rail"
)

// ReplayStep is one moment in a payment's life, ready to animate.
type ReplayStep struct {
	Index      int
	At         time.Time
	OffsetMS   int64  // since the first step
	DeltaMS    int64  // since the previous step
	DurationMS *int64 // for attempts that finished
	Type       string
	Actor      string
	FromState  string // transitions
	ToState    string // transitions
	Title      string
	Event      *domain.Event // transitions
	Attempt    *Attempt      // submissions and rail calls
}

// Replay is every step a payment went through, in order.
type Replay struct {
	Payment    Payment
	StartedAt  time.Time
	EndedAt    time.Time
	DurationMS int64
	States     []string // the state path, from the events
	ChainValid bool
	ChainError error
	Steps      []ReplayStep
}

// BuildReplay merges a payment's events and attempts into one timeline.
// Events and attempts are stamped by the same (database) clock. On a
// tie, a submission comes before a transition before a rail call, and
// events keep their sequence order.
func BuildReplay(d PaymentDetail) Replay {
	r := Replay{Payment: d.Payment, ChainValid: d.ChainError == nil, ChainError: d.ChainError}

	type entry struct {
		step     ReplayStep
		priority int
		seq      int
	}
	var entries []entry
	state := ""
	for i := range d.Events {
		e := d.Events[i]
		entries = append(entries, entry{step: transitionStep(e, state), priority: 1, seq: e.SequenceNumber})
		r.States = append(r.States, e.Type)
		state = e.Type
	}
	for i := range d.Attempts {
		a := d.Attempts[i]
		step, priority := attemptStep(a)
		entries = append(entries, entry{step: step, priority: priority, seq: i})
	}
	sort.SliceStable(entries, func(i, j int) bool {
		a, b := entries[i], entries[j]
		if !a.step.At.Equal(b.step.At) {
			return a.step.At.Before(b.step.At)
		}
		if a.priority != b.priority {
			return a.priority < b.priority
		}
		return a.seq < b.seq
	})

	for i, e := range entries {
		s := e.step
		s.Index = i
		if i > 0 {
			s.OffsetMS = s.At.Sub(entries[0].step.At).Milliseconds()
			s.DeltaMS = s.At.Sub(r.Steps[i-1].At).Milliseconds()
		}
		r.Steps = append(r.Steps, s)
	}
	if len(r.Steps) > 0 {
		r.StartedAt, r.EndedAt = r.Steps[0].At, r.Steps[len(r.Steps)-1].At
		r.DurationMS = r.EndedAt.Sub(r.StartedAt).Milliseconds()
	}
	return r
}

func transitionStep(e domain.Event, from string) ReplayStep {
	s := ReplayStep{At: e.RecordedAt, Type: StepTransition, FromState: from, ToState: e.Type, Event: &e}
	switch domain.State(e.Type) {
	case domain.PROPOSED:
		s.Actor, s.Title = ActorWorkflow, "Payment proposed: the durable workflow took it on"
	case domain.AUTHORIZED:
		s.Actor, s.Title = ActorPolicy, "Policy authorised the payment"
	case domain.BLOCKED:
		s.Actor, s.Title = ActorPolicy, "Policy blocked the payment; Airwallex was never called"
	case domain.IN_FLIGHT:
		s.Actor, s.Title = ActorWorkflow, "Recorded as in flight, before calling Airwallex"
		if e.Payload != "" {
			s.Actor, s.Title = ActorWorkflow, "In flight again: the lookup found the transfer already on Airwallex"
		}
	case domain.UNKNOWN:
		s.Actor, s.Title = ActorWorkflow, "Outcome unclear: looking the transfer up before anything is retried"
	case domain.APPLIED:
		s.Actor, s.Title = ActorRail, "Airwallex confirmed the transfer paid"
	case domain.FAILED:
		s.Actor, s.Title = ActorRail, "Airwallex rejected or cancelled the transfer"
	default:
		s.Actor, s.Title = ActorWorkflow, "Moved to "+e.Type
	}
	return s
}

func attemptStep(a Attempt) (ReplayStep, int) {
	s := ReplayStep{At: a.StartedAt, Attempt: &a}
	if d := a.Duration(); d != nil {
		ms := d.Milliseconds()
		s.DurationMS = &ms
	}
	if a.Kind == KindAgentSubmission {
		s.Type, s.Actor, s.Title = StepSubmission, ActorAgent, submissionTitle(a)
		return s, 0
	}
	s.Type, s.Actor, s.Title = StepRailCall, ActorRail, railCallTitle(a)
	return s, 2
}

func submissionTitle(a Attempt) string {
	who := "An agent"
	if a.AgentName != "" {
		who = "Agent " + a.AgentName
	}
	switch a.Outcome {
	case OutcomeStarted:
		return who + " submitted the intent; a new payment started"
	case OutcomeAttached:
		return who + " submitted it again while it was starting; joined the same run"
	case OutcomeDeduplicated:
		return fmt.Sprintf("%s submitted it again; recognised as a duplicate, nothing new started (%s)", who, a.Details["reported_state"])
	case OutcomeInvalid:
		return who + " submitted an invalid intent: " + a.ErrorMessage
	case OutcomePayeeUnknown:
		return who + " named a payee this organisation never registered; refused"
	default:
		return who + " submitted the intent; Enquoir could not process it"
	}
}

func railCallTitle(a Attempt) string {
	var what string
	switch a.Operation {
	case OpCreateTransfer:
		what = "Create transfer"
	case OpGetTransfer:
		what = "Check transfer"
	case OpFindTransfer:
		what = "Look up transfer by request ID"
	case OpCreateBeneficiary:
		what = "Create beneficiary"
	case OpFindBeneficiary:
		what = "Look up beneficiary"
	default:
		what = a.Operation
	}
	if a.ActivityAttempt > 1 {
		what += fmt.Sprintf(" (retry #%d)", a.ActivityAttempt-1)
	}

	switch a.Outcome {
	case OutcomeOK:
		switch a.Operation {
		case OpCreateTransfer:
			return fmt.Sprintf("%s: Airwallex created %s (%s)", what, a.TransferID, a.TransferStatus)
		case OpFindTransfer:
			return fmt.Sprintf("%s: found %s (%s)", what, a.TransferID, a.TransferStatus)
		case OpGetTransfer:
			return fmt.Sprintf("%s: %s", what, a.TransferStatus)
		}
		return what + ": done"
	case OutcomeNotFound:
		if a.Operation == OpFindTransfer {
			return what + ": no transfer exists, so it is safe to send"
		}
		return what + ": none found"
	case OutcomeRejected:
		return fmt.Sprintf("%s: Airwallex said no%s", what, codeSuffix(a))
	case OutcomeAmbiguous:
		return fmt.Sprintf("%s: outcome unclear%s", what, codeSuffix(a))
	case OutcomeDuplicateTransfers:
		return what + ": more than one transfer for one request ID; stopped for a person to investigate"
	case OutcomeInProgress:
		return what + ": under way"
	case OutcomeInterrupted:
		return what + ": the result was never recorded; the worker likely stopped mid-call"
	default:
		return fmt.Sprintf("%s: failed%s", what, codeSuffix(a))
	}
}

func codeSuffix(a Attempt) string {
	switch {
	case a.HTTPStatus != 0 && a.ErrorCode != "":
		return fmt.Sprintf(" (HTTP %d, %s)", a.HTTPStatus, a.ErrorCode)
	case a.HTTPStatus != 0:
		return fmt.Sprintf(" (HTTP %d)", a.HTTPStatus)
	case a.ErrorCode != "":
		return " (" + a.ErrorCode + ")"
	}
	return ""
}

// ReplayPayment builds a payment's replay.
type ReplayPayment struct{ Get *GetPayment }

func (r *ReplayPayment) Run(ctx context.Context, org domain.OrgID, key domain.IntentKey) (Replay, error) {
	d, err := r.Get.Run(ctx, org, key)
	if err != nil {
		return Replay{}, err
	}
	return BuildReplay(d), nil
}
