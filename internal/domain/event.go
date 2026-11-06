package domain

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// genesisHash is the PrevHash of the first event in any chain.
const genesisHash = ""

// Event is one durably recorded step in an Intent's execution. Write
// before you call: an Event exists before the outbound request it
// describes ever leaves.
type Event struct {
	SequenceNumber int
	IntentKey      IntentKey
	Type           string
	Payload        string
	PrevHash       string
	Hash           string

	// RecordedAt is when the event was stored. It is not part of the
	// hash: the chain proves order and content, not wall-clock time.
	RecordedAt time.Time
}

func hashEvent(seq int, intentKey IntentKey, eventType, payload, prevHash string) string {
	const sep = "\x00"
	canonical := strings.Join([]string{
		strconv.Itoa(seq),
		string(intentKey),
		eventType,
		payload,
		prevHash,
	}, sep)
	sum := sha256.Sum256([]byte(canonical))
	return hex.EncodeToString(sum[:])
}

// Chain is an ordered, hash-linked sequence of Events, starting at
// sequence 0. It can be verified independently of how it was produced -
// loaded fresh from storage, it carries everything needed to detect
// tampering or a missing step.
type Chain []Event

// Append adds the next event to the chain, linking it to the
// previous event's hash.
func (c Chain) Append(intentKey IntentKey, eventType, payload string) Chain {
	var last *Event
	if len(c) > 0 {
		last = &c[len(c)-1]
	}
	return append(c, NextEvent(intentKey, last, eventType, payload))
}

// NextEvent builds the event that follows last (nil for the first),
// from last alone: a store can append without reading the whole chain.
func NextEvent(intentKey IntentKey, last *Event, eventType, payload string) Event {
	seq, prevHash := 0, genesisHash
	if last != nil {
		seq, prevHash = last.SequenceNumber+1, last.Hash
	}
	return Event{
		SequenceNumber: seq,
		IntentKey:      intentKey,
		Type:           eventType,
		Payload:        payload,
		PrevHash:       prevHash,
		Hash:           hashEvent(seq, intentKey, eventType, payload, prevHash),
	}
}

// Verify walks the chain from its start, confirming sequence numbers are
// gapless, each event's hash matches its own content, and each event
// links to the previous event's hash.
func (c Chain) Verify() error {
	prevHash := genesisHash
	for i, e := range c {
		if e.SequenceNumber != i {
			return fmt.Errorf("missing event: expected sequence %d, got %d", i, e.SequenceNumber)
		}
		if e.PrevHash != prevHash {
			return fmt.Errorf("chain broken at sequence %d: previous hash does not match", e.SequenceNumber)
		}
		if want := hashEvent(e.SequenceNumber, e.IntentKey, e.Type, e.Payload, e.PrevHash); e.Hash != want {
			return fmt.Errorf("event at sequence %d has been altered: hash mismatch", e.SequenceNumber)
		}
		prevHash = e.Hash
	}
	return nil
}
