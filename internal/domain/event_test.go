package domain

import (
	"strings"
	"testing"
)

func buildChain(t *testing.T) Chain {
	t.Helper()
	var chain Chain
	chain = chain.Append("intent-key-1", "PROPOSED", "payload-0")
	chain = chain.Append("intent-key-1", "AUTHORIZED", "payload-1")
	chain = chain.Append("intent-key-1", "IN_FLIGHT", "payload-2")
	return chain
}

func TestChain_ValidChainVerifies(t *testing.T) {
	chain := buildChain(t)
	if err := chain.Verify(); err != nil {
		t.Errorf("valid chain failed to verify: %v", err)
	}
}

func TestChain_EditedEventFailsVerificationAtItsSequence(t *testing.T) {
	chain := buildChain(t)
	chain[1].Payload = "tampered"

	err := chain.Verify()
	if err == nil {
		t.Fatal("expected tampering to fail verification, got nil error")
	}
	if !strings.Contains(err.Error(), "sequence 1") {
		t.Errorf("error does not identify sequence 1: %v", err)
	}
}

func TestChain_MissingEventIsDetected(t *testing.T) {
	chain := buildChain(t)
	withGap := Chain{chain[0], chain[2]} // drop sequence 1

	err := withGap.Verify()
	if err == nil {
		t.Fatal("expected missing event to fail verification, got nil error")
	}
	if !strings.Contains(err.Error(), "sequence 1") {
		t.Errorf("error does not identify the missing sequence 1: %v", err)
	}
}

func TestChain_EmptyChainVerifies(t *testing.T) {
	var chain Chain
	if err := chain.Verify(); err != nil {
		t.Errorf("empty chain should trivially verify: %v", err)
	}
}

// NextEvent builds the event that follows last knowing only last, so a
// store can append without reading the whole chain. It must be exactly
// what Chain.Append would add.
func TestNextEvent_MatchesChainAppend(t *testing.T) {
	const key = IntentKey("k1")
	if got, want := NextEvent(key, nil, "PROPOSED", ""), (Chain{}).Append(key, "PROPOSED", "")[0]; got != want {
		t.Errorf("first event: got %+v, want %+v", got, want)
	}
	chain := Chain{}.Append(key, "PROPOSED", "").Append(key, "AUTHORIZED", "").Append(key, "IN_FLIGHT", "")
	last := chain[len(chain)-1]
	want := chain.Append(key, "APPLIED", "tr_1")
	if got := NextEvent(key, &last, "APPLIED", "tr_1"); got != want[len(want)-1] {
		t.Errorf("next event: got %+v, want %+v", got, want[len(want)-1])
	}
	if err := append(chain, NextEvent(key, &last, "APPLIED", "tr_1")).Verify(); err != nil {
		t.Errorf("a chain extended by NextEvent does not verify: %v", err)
	}
}
