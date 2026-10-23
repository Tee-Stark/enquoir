package airwallex

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"

	"enquoir/internal/app"
	"enquoir/internal/domain"
)

var _ app.Rail = (*Client)(nil)

// Client is the Rail implementation: it moves money, and registers the
// payees money moves to (BeneficiaryRegistry). Sandbox-only simulation
// calls live on the separate Simulator type.
type Client struct {
	t *transport
}

// NewClient refuses to build a Client against anything but the sandbox.
func NewClient(baseURL, clientID, apiKey string, httpClient *http.Client, clock app.Clock) (*Client, error) {
	if err := requireSandbox(baseURL); err != nil {
		return nil, err
	}
	return newClientWithBaseURL(baseURL, clientID, apiKey, httpClient, clock)
}

// newClientWithBaseURL skips the sandbox check, for tests driving a
// local httptest server.
func newClientWithBaseURL(baseURL, clientID, apiKey string, httpClient *http.Client, clock app.Clock) (*Client, error) {
	return &Client{t: newTransport(baseURL, clientID, apiKey, httpClient, clock)}, nil
}

type transferDTO struct {
	ID            string `json:"id"`
	RequestID     string `json:"request_id"`
	Status        string `json:"status"`
	FailureType   string `json:"failure_type"`
	FailureReason string `json:"failure_reason"`
}

func (dto transferDTO) toTransfer() app.Transfer {
	return app.Transfer{
		ID:            dto.ID,
		RequestID:     dto.RequestID,
		Status:        app.TransferStatus(dto.Status),
		FailureType:   dto.FailureType,
		FailureReason: dto.FailureReason,
	}
}

type errorDTO struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// CreateTransfer asks the rail to pay intent, using intent.Key as the
// idempotency key. A rejected create maps to ErrRejected; anything
// whose outcome can't be determined maps to ErrAmbiguous. Reusing a
// request ID that the rail already has on file resolves to the
// existing transfer instead of erroring - that is what idempotency
// means here, not a special case the caller should have to handle.
func (c *Client) CreateTransfer(ctx context.Context, intent domain.Intent) (app.Transfer, error) {
	requestID := string(intent.Key)

	status, body, err := c.t.doJSON(ctx, http.MethodPost, "/api/v1/transfers/create", map[string]any{
		"request_id":        requestID,
		"beneficiary_id":    intent.Payee,
		"transfer_currency": string(intent.Amount.Currency()),
		"source_currency":   string(intent.Amount.Currency()),
		"transfer_amount":   intent.Amount.MajorUnitsString(),
		"transfer_method":   "LOCAL",
		"reason":            "other",
		"reference":         intent.Reference,
	})
	if err != nil {
		return app.Transfer{}, err
	}

	if status >= 200 && status < 300 {
		var dto transferDTO
		if err := json.Unmarshal(body, &dto); err != nil {
			return app.Transfer{}, fmt.Errorf("airwallex: create transfer: unparseable response %s: %w", body, app.ErrAmbiguous)
		}
		return dto.toTransfer(), nil
	}

	if status >= 500 {
		return app.Transfer{}, fmt.Errorf("airwallex: create transfer: status %d: %s: %w", status, body, app.ErrAmbiguous)
	}

	var errResp errorDTO
	_ = json.Unmarshal(body, &errResp)

	if errResp.Code == "duplicate_request_id" {
		transfer, ok, err := c.FindTransferByRequestID(ctx, requestID)
		if err != nil {
			return app.Transfer{}, err
		}
		if !ok {
			return app.Transfer{}, fmt.Errorf("airwallex: create transfer: duplicate_request_id but no transfer found for %s: %w", requestID, app.ErrAmbiguous)
		}
		return transfer, nil
	}

	return app.Transfer{}, fmt.Errorf("airwallex: create transfer: rejected, status %d: %s: %w", status, body, app.ErrRejected)
}

// GetTransfer fetches a transfer by the rail's own ID.
func (c *Client) GetTransfer(ctx context.Context, transferID string) (app.Transfer, error) {
	status, body, err := c.t.doJSON(ctx, http.MethodGet, "/api/v1/transfers/"+transferID, nil)
	if err != nil {
		return app.Transfer{}, err
	}
	if status != http.StatusOK {
		return app.Transfer{}, fmt.Errorf("airwallex: get transfer %s: status %d: %s: %w", transferID, status, body, app.ErrAmbiguous)
	}

	var dto transferDTO
	if err := json.Unmarshal(body, &dto); err != nil {
		return app.Transfer{}, fmt.Errorf("airwallex: get transfer %s: unparseable response %s: %w", transferID, body, app.ErrAmbiguous)
	}
	return dto.toTransfer(), nil
}

// FindTransferByRequestID looks a transfer up by the request ID it was
// created with - the only safe way to resolve an ambiguous outcome
// before ever retrying.
func (c *Client) FindTransferByRequestID(ctx context.Context, requestID string) (app.Transfer, bool, error) {
	path := "/api/v1/transfers?request_id=" + url.QueryEscape(requestID)
	status, body, err := c.t.doJSON(ctx, http.MethodGet, path, nil)
	if err != nil {
		return app.Transfer{}, false, err
	}
	if status != http.StatusOK {
		return app.Transfer{}, false, fmt.Errorf("airwallex: find transfer by request id %s: status %d: %s: %w", requestID, status, body, app.ErrAmbiguous)
	}

	var list struct {
		Items []transferDTO `json:"items"`
	}
	if err := json.Unmarshal(body, &list); err != nil {
		return app.Transfer{}, false, fmt.Errorf("airwallex: find transfer by request id %s: unparseable response %s: %w", requestID, body, app.ErrAmbiguous)
	}
	// Only an exact request_id match counts, since the list endpoint is
	// only documented to filter on it, not to match it exactly. More
	// than one match means a double payment already exists on the rail
	// and must surface, not be hidden behind whichever came first.
	var matches []transferDTO
	for _, item := range list.Items {
		if item.RequestID == requestID {
			matches = append(matches, item)
		}
	}
	switch len(matches) {
	case 0:
		return app.Transfer{}, false, nil
	case 1:
		return matches[0].toTransfer(), true, nil
	default:
		ids := make([]string, len(matches))
		for i, m := range matches {
			ids[i] = m.ID
		}
		return app.Transfer{}, false, fmt.Errorf("airwallex: find transfer by request id %s: transfers %v: %w", requestID, ids, app.ErrDuplicateTransfers)
	}
}
