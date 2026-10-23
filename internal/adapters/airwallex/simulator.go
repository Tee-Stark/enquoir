package airwallex

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"

	"enquoir/internal/app"
	"enquoir/internal/domain"
)

var _ app.Simulator = (*Simulator)(nil)

// Simulator drives sandbox-only state changes - funding an account and
// forcing a transfer through its states. It is a distinct type from
// Client with its own transport, so holding only a Rail can never
// reach a simulation call.
type Simulator struct {
	t *transport
}

// NewSimulator refuses to build a Simulator against anything but the
// sandbox.
func NewSimulator(baseURL, clientID, apiKey string, httpClient *http.Client, clock app.Clock) (*Simulator, error) {
	if err := requireSandbox(baseURL); err != nil {
		return nil, err
	}
	return newSimulatorWithBaseURL(baseURL, clientID, apiKey, httpClient, clock)
}

func newSimulatorWithBaseURL(baseURL, clientID, apiKey string, httpClient *http.Client, clock app.Clock) (*Simulator, error) {
	return &Simulator{t: newTransport(baseURL, clientID, apiKey, httpClient, clock)}, nil
}

// FundAccount ensures a Global Account exists for amount's currency and
// deposits amount into it via the deposit simulation.
func (s *Simulator) FundAccount(ctx context.Context, amount domain.Money) error {
	accountID, err := s.ensureGlobalAccount(ctx, amount.Currency())
	if err != nil {
		return err
	}

	status, body, err := s.t.doJSON(ctx, http.MethodPost, "/api/v1/simulation/deposit/create", map[string]any{
		"global_account_id": accountID,
		// A JSON number, as the docs type it, built from the exact
		// decimal string so no float is ever involved.
		"amount":     json.Number(amount.MajorUnitsString()),
		"payer_name": "Enquoir",
	})
	if err != nil {
		return err
	}
	if status < 200 || status >= 300 {
		return fmt.Errorf("airwallex: simulate deposit: status %d: %s: %w", status, body, app.ErrAmbiguous)
	}
	return nil
}

// ensureGlobalAccount lists existing Global Accounts and reuses one
// whose required_features include currency - the real list response
// has no top-level currency field - creating one only if none exists.
// Field names follow the Airwallex docs and the probe's observations.
func (s *Simulator) ensureGlobalAccount(ctx context.Context, currency domain.Currency) (string, error) {
	status, body, err := s.t.doJSON(ctx, http.MethodGet, "/api/v1/global_accounts", nil)
	if err != nil {
		return "", err
	}
	if status == http.StatusOK {
		var list struct {
			Items []struct {
				ID               string `json:"id"`
				RequiredFeatures []struct {
					Currency string `json:"currency"`
				} `json:"required_features"`
			} `json:"items"`
		}
		if err := json.Unmarshal(body, &list); err == nil {
			for _, item := range list.Items {
				for _, f := range item.RequiredFeatures {
					if f.Currency == string(currency) {
						return item.ID, nil
					}
				}
			}
		}
	}

	// A fixed request_id per currency, so a retried create is
	// recognised by the rail rather than opening a second account.
	status, body, err = s.t.doJSON(ctx, http.MethodPost, "/api/v1/global_accounts/create", map[string]any{
		"country_code": "US",
		"request_id":   "enquoir-global-account-" + string(currency),
		"nick_name":    "Enquoir " + string(currency),
		"required_features": []map[string]any{
			{"currency": string(currency), "transfer_method": "LOCAL"},
		},
	})
	if err != nil {
		return "", err
	}
	if status < 200 || status >= 300 {
		return "", fmt.Errorf("airwallex: create global account: status %d: %s: %w", status, body, app.ErrAmbiguous)
	}

	var created struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(body, &created); err != nil || created.ID == "" {
		return "", fmt.Errorf("airwallex: create global account: unparseable response %s: %w", body, app.ErrAmbiguous)
	}
	return created.ID, nil
}

// TransitionTransfer forces transferID into toStatus through the
// sandbox's simulation endpoint, whose body field is next_status.
func (s *Simulator) TransitionTransfer(ctx context.Context, transferID string, toStatus app.TransferStatus) error {
	status, body, err := s.t.doJSON(ctx, http.MethodPost, "/api/v1/simulation/transfers/"+transferID+"/transition", map[string]any{
		"next_status": string(toStatus),
	})
	if err != nil {
		return err
	}
	if status < 200 || status >= 300 {
		return fmt.Errorf("airwallex: transition transfer %s to %s: status %d: %s: %w", transferID, toStatus, status, body, app.ErrAmbiguous)
	}
	return nil
}
