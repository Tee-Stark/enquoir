// Command probe is throwaway exploratory tooling: it calls the real
// Airwallex sandbox and prints exactly what comes back, so the adapter's
// tests can be shaped by observed behaviour instead of guesses. Findings
// belong in docs/rail-notes.md, not in this file. Exempt from test-first.
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"regexp"
	"time"
)

const wantBaseURL = "https://api.sandbox.airwallex.com"

func main() {
	baseURL := os.Getenv("AWX_BASE_URL")
	if baseURL != wantBaseURL {
		log.Fatalf("refusing to run: AWX_BASE_URL=%q, want %q - real money must be impossible", baseURL, wantBaseURL)
	}
	clientID := os.Getenv("AWX_CLIENT_ID")
	apiKey := os.Getenv("AWX_API_KEY")
	if clientID == "" || apiKey == "" {
		log.Fatal("refusing to run: AWX_CLIENT_ID and AWX_API_KEY must both be set")
	}

	c := &client{baseURL: baseURL, http: &http.Client{Timeout: 30 * time.Second}}

	section("1. Login")
	c.token = c.login(clientID, apiKey)

	section("2. Ensure a funded USD Global Account")
	accountID := c.ensureGlobalAccount()
	c.simulateDeposit(accountID)

	section("3. Beneficiary schema for US / USD / LOCAL")
	c.beneficiarySchema()

	section("4. Create a beneficiary")
	beneficiaryID := c.createBeneficiary()

	section("5. Create a transfer")
	requestID := fmt.Sprintf("probe-%d", time.Now().UnixNano())
	transferID := c.createTransfer(requestID, beneficiaryID, "1.00")

	section("6. Retry the same create-transfer call with the SAME request ID")
	c.createTransfer(requestID, beneficiaryID, "1.00")

	section("7. Reuse the SAME request ID for a DIFFERENT transfer (different amount)")
	c.createTransfer(requestID, beneficiaryID, "2.00")

	section("8. Look the transfer up by request ID")
	c.findByRequestID(requestID)

	section("9. Fail the transfer through the simulator")
	c.transitionTransfer(transferID, "SENT")
	c.transitionTransfer(transferID, "FAILED")

	section("10. Re-fetch the transfer after failing it")
	c.getTransfer(transferID)

	fmt.Println("\nProbe complete. Transcribe findings into docs/rail-notes.md.")
}

func section(title string) {
	fmt.Printf("\n=== %s ===\n", title)
}

type client struct {
	baseURL string
	token   string
	http    *http.Client
}

// do makes a request and returns the raw response body and status code,
// printing both. It never treats a non-2xx response as fatal: the whole
// point of this tool is to see what the sandbox actually says.
func (c *client) do(method, path string, body any, headers map[string]string) (int, []byte) {
	var reader io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			log.Fatalf("marshalling request body for %s: %v", path, err)
		}
		reader = bytes.NewReader(b)
		fmt.Printf("--> %s %s\n    body: %s\n", method, path, b)
	} else {
		fmt.Printf("--> %s %s\n", method, path)
	}

	req, err := http.NewRequest(method, c.baseURL+path, reader)
	if err != nil {
		log.Fatalf("building request for %s: %v", path, err)
	}
	req.Header.Set("Content-Type", "application/json")
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		fmt.Printf("<-- transport error: %v\n", err)
		return 0, nil
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		log.Fatalf("reading response for %s: %v", path, err)
	}
	fmt.Printf("<-- %d %s\n", resp.StatusCode, redactTokens(respBody))
	return resp.StatusCode, respBody
}

var tokenField = regexp.MustCompile(`"token"\s*:\s*"[^"]*"`)

// redactTokens hides bearer tokens from printed output. The login
// response carries one, and probe output gets pasted into notes.
func redactTokens(body []byte) []byte {
	return tokenField.ReplaceAll(body, []byte(`"token":"<redacted>"`))
}

func (c *client) login(clientID, apiKey string) string {
	status, body := c.do(http.MethodPost, "/api/v1/authentication/login", nil, map[string]string{
		"x-client-id": clientID,
		"x-api-key":   apiKey,
	})
	if status != 200 {
		log.Fatalf("login failed with status %d", status)
	}

	var parsed struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		log.Fatalf("parsing login response: %v", err)
	}
	if parsed.Token == "" {
		log.Fatal("login response had no top-level \"token\" field - inspect the raw body above and fix this probe")
	}
	return parsed.Token
}

func (c *client) ensureGlobalAccount() string {
	_, listBody := c.do(http.MethodGet, "/api/v1/global_accounts", nil, nil)

	var list struct {
		Items []struct {
			ID       string `json:"id"`
			Currency string `json:"currency"`
		} `json:"items"`
	}
	if err := json.Unmarshal(listBody, &list); err == nil {
		for _, item := range list.Items {
			if item.Currency == "USD" {
				fmt.Printf("    reusing existing USD global account %s\n", item.ID)
				return item.ID
			}
		}
	}

	_, createBody := c.do(http.MethodPost, "/api/v1/global_accounts/create", map[string]any{
		"country_code":    "US",
		"currency":        "USD",
		"transfer_method": "LOCAL",
	}, nil)

	var created struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(createBody, &created); err != nil || created.ID == "" {
		log.Fatal("could not determine the new global account's id - inspect the raw body above")
	}
	return created.ID
}

func (c *client) simulateDeposit(globalAccountID string) {
	c.do(http.MethodPost, "/api/v1/simulation/deposit/create", map[string]any{
		"global_account_id": globalAccountID,
		"amount":            100000,
		"payer_name":        "Enquoir Probe",
	}, nil)
}

func (c *client) beneficiarySchema() {
	c.do(http.MethodPost, "/api/v1/beneficiaries/schema", map[string]any{
		"bank_country_code": "US",
		"currency":          "USD",
		"transfer_method":   "LOCAL",
		"entity_type":       "COMPANY",
	}, nil)
}

func (c *client) createBeneficiary() string {
	_, body := c.do(http.MethodPost, "/api/v1/beneficiaries/create", map[string]any{
		"beneficiary": map[string]any{
			"entity_type":  "COMPANY",
			"company_name": "Enquoir Probe Test Payee",
			"bank_details": map[string]any{
				"account_name":           "Enquoir Probe Test Payee",
				"account_number":         "000123456789",
				"account_currency":       "USD",
				"bank_country_code":      "US",
				"account_routing_type1":  "aba",
				"account_routing_value1": "021000021",
				"local_clearing_system":  "ACH",
			},
			"address": map[string]any{
				"street_address": "412 5th Avenue",
				"city":           "Seattle",
				"state":          "WA",
				"postcode":       "98104",
				"country_code":   "US",
			},
		},
		"transfer_methods": []string{"LOCAL"},
	}, nil)

	var created struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(body, &created); err != nil || created.ID == "" {
		log.Fatal("could not determine the new beneficiary's id - inspect the raw body above")
	}
	return created.ID
}

func (c *client) createTransfer(requestID, beneficiaryID, amount string) string {
	_, body := c.do(http.MethodPost, "/api/v1/transfers/create", map[string]any{
		"request_id":        requestID,
		"beneficiary_id":    beneficiaryID,
		"transfer_currency": "USD",
		"source_currency":   "USD",
		"transfer_amount":   amount,
		"transfer_method":   "LOCAL",
		"reason":            "other",
		"reference":         "enquoir probe",
	}, nil)

	var created struct {
		ID string `json:"id"`
	}
	json.Unmarshal(body, &created)
	return created.ID
}

func (c *client) findByRequestID(requestID string) {
	c.do(http.MethodGet, "/api/v1/transfers?request_id="+requestID, nil, nil)
}

func (c *client) getTransfer(transferID string) {
	if transferID == "" {
		fmt.Println("    (no transfer id known, skipping)")
		return
	}
	c.do(http.MethodGet, "/api/v1/transfers/"+transferID, nil, nil)
}

func (c *client) transitionTransfer(transferID, toStatus string) {
	if transferID == "" {
		fmt.Println("    (no transfer id known, skipping)")
		return
	}
	c.do(http.MethodPost, "/api/v1/simulation/transfers/"+transferID+"/transition", map[string]any{
		"status": toStatus,
	}, nil)
}
