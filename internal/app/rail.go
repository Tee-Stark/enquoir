package app

import (
	"context"
	"errors"
	"time"

	"enquoir/internal/domain"
)

// ErrRejected means the rail gave a clear, final no: the transfer was
// never created and never will be from this request.
var ErrRejected = errors.New("rail: transfer rejected")

// ErrAmbiguous means the outcome of a call is unknown - a timeout, a
// dropped connection, or a server error. The caller must look the
// transfer up by request ID before ever retrying.
var ErrAmbiguous = errors.New("rail: outcome ambiguous")

// TransferStatus is a rail-reported transfer status. These are the
// rail's own vocabulary, not domain.State: SENT means in flight, not
// final, and a failed transfer ends as CANCELLED, never "FAILED".
type TransferStatus string

const (
	TransferSent      TransferStatus = "SENT"
	TransferPaid      TransferStatus = "PAID"
	TransferCancelled TransferStatus = "CANCELLED"
)

// Transfer is a rail's view of one payment. FailureType and
// FailureReason are only meaningful when Status is TransferCancelled.
type Transfer struct {
	ID            string
	RequestID     string
	Status        TransferStatus
	FailureType   string
	FailureReason string
}

// Rail moves money through a payment rail. Every mutating call is keyed
// by a request ID - the Intent's key - so a resubmission is recognised
// rather than paid twice. A sandbox-only rail must refuse to start
// against any other base URL.
type Rail interface {
	// CreateTransfer asks the rail to pay intent, using intent.Key as
	// the request ID. Returns ErrRejected for a clear no, or
	// ErrAmbiguous when the outcome could not be determined - the
	// caller must then use FindTransferByRequestID, never retry blind.
	CreateTransfer(ctx context.Context, intent domain.Intent) (Transfer, error)

	// GetTransfer fetches a transfer by the rail's own ID.
	GetTransfer(ctx context.Context, transferID string) (Transfer, error)

	// FindTransferByRequestID looks a transfer up by the request ID it
	// was created with. ok is false when no such transfer exists.
	FindTransferByRequestID(ctx context.Context, requestID string) (transfer Transfer, ok bool, err error)
}

// Simulator drives sandbox-only state changes that a real rail would
// never expose: funding an account and forcing a transfer through its
// states. It is a separate interface from Rail so that nothing which
// depends only on Rail can accidentally reach a sandbox-only call.
type Simulator interface {
	// FundAccount deposits amount into the sandbox's account for
	// amount's currency.
	FundAccount(ctx context.Context, amount domain.Money) error

	// TransitionTransfer forces transferID into toStatus.
	TransitionTransfer(ctx context.Context, transferID string, toStatus TransferStatus) error
}

// Clock supplies the current time to application-layer code, so that
// code stays testable with a fake instead of calling time.Now directly.
type Clock interface {
	Now() time.Time
}
