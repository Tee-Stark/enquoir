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

var _ app.BeneficiaryRegistry = (*Client)(nil)

// CreateBeneficiary registers b as a US company paid in USD over ACH.
// The create endpoint takes no request_id, so b's key goes in as the
// beneficiary's nickname instead - FindBeneficiary looks it up by that
// nickname to resolve an ambiguous create before anything is retried.
// Field names follow POST /api/v1/beneficiaries/create in the
// Airwallex docs; required fields and formats follow the live
// US/USD/LOCAL/ACH form schema (POST /beneficiary_form_schemas/generate).
func (c *Client) CreateBeneficiary(ctx context.Context, b domain.Beneficiary) (string, error) {
	status, body, err := c.t.doJSON(ctx, http.MethodPost, "/api/v1/beneficiaries/create", map[string]any{
		"nickname":         string(b.Key),
		"transfer_methods": []string{"LOCAL"},
		"beneficiary": map[string]any{
			"entity_type":  "COMPANY",
			"company_name": b.Name,
			"address": map[string]any{
				"street_address": b.Address.Street,
				"city":           b.Address.City,
				"state":          "US-" + b.Address.State, // the schema wants ISO 3166-2
				"postcode":       b.Address.Postcode,
				"country_code":   "US",
			},
			"bank_details": map[string]any{
				"account_currency":       "USD",
				"account_name":           b.Name,
				"account_number":         b.AccountNumber,
				"account_routing_type1":  "aba",
				"account_routing_value1": b.RoutingNumber,
				"bank_account_category":  string(b.Category),
				"bank_country_code":      "US",
				"local_clearing_system":  "ACH",
			},
		},
	})
	if err != nil {
		return "", err
	}

	if status >= 500 {
		return "", fmt.Errorf("airwallex: create beneficiary: status %d: %s: %w", status, body, app.ErrAmbiguous)
	}
	if status < 200 || status >= 300 {
		return "", fmt.Errorf("airwallex: create beneficiary: rejected, status %d: %s: %w", status, body, app.ErrRejected)
	}

	var created struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(body, &created); err != nil || created.ID == "" {
		return "", fmt.Errorf("airwallex: create beneficiary: unparseable response %s: %w", body, app.ErrAmbiguous)
	}
	return created.ID, nil
}

// FindBeneficiary looks a beneficiary up by the key CreateBeneficiary
// stored as its nickname. Only an exact nickname match counts, since
// the list endpoint is only documented to filter on nickname, not to
// match it exactly.
func (c *Client) FindBeneficiary(ctx context.Context, key domain.BeneficiaryKey) (string, bool, error) {
	path := "/api/v1/beneficiaries?nick_name=" + url.QueryEscape(string(key))
	status, body, err := c.t.doJSON(ctx, http.MethodGet, path, nil)
	if err != nil {
		return "", false, err
	}
	if status != http.StatusOK {
		return "", false, fmt.Errorf("airwallex: find beneficiary %s: status %d: %s: %w", key, status, body, app.ErrAmbiguous)
	}

	var list struct {
		Items []struct {
			ID       string `json:"id"`
			Nickname string `json:"nickname"`
		} `json:"items"`
	}
	if err := json.Unmarshal(body, &list); err != nil {
		return "", false, fmt.Errorf("airwallex: find beneficiary %s: unparseable response %s: %w", key, body, app.ErrAmbiguous)
	}
	for _, item := range list.Items {
		if item.Nickname == string(key) {
			return item.ID, true, nil
		}
	}
	return "", false, nil
}
