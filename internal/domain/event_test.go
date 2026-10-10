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
