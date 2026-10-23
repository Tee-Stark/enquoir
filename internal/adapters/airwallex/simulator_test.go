package airwallex

import (
	"bytes"
	"encoding/json"
	"net/http"
	"reflect"
	"testing"
	"time"

	"enquoir/internal/domain"
)

func newTestSimulator(t *testing.T, handle func(w http.ResponseWriter, r *http.Request)) *Simulator {
	t.Helper()
	ts, _ := server(t, handle)
	s, err := newSimulatorWithBaseURL(ts.URL, "client-id", "api-key", ts.Client(), &fakeClock{now: time.Now()})
	if err != nil {
		t.Fatalf("building test simulator: %v", err)
	}
	s.t.sleep = noSleep
	return s
}

// decodeNumbers keeps JSON numbers as their exact text, so a test can
// tell 1000.00 sent as a number from "1000.00" sent as a string.
func decodeNumbers(t *testing.T, body []byte) map[string]any {
	t.Helper()
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	var m map[string]any
	if err := dec.Decode(&m); err != nil {
		t.Fatalf("decoding %s: %v", body, err)
	}
	return m
}

// globalAccountsList is shaped like the real sandbox's response, as
// observed by cmd/probe: no top-level currency - it lives in
// required_features.
const globalAccountsList = `{"items":[
	{"id":"ga_hkd","country_code":"HK","status":"PROCESSING","required_features":[{"currency":"HKD","transfer_method":"LOCAL"}]},
	{"id":"ga_usd","country_code":"US","status":"ACTIVE","required_features":[{"currency":"USD","transfer_method":"LOCAL"}]}
]}`

func TestSimulator_FundAccount_DepositsIntoExistingAccountForCurrency(t *testing.T) {
	var deposit map[string]any
	s := newTestSimulator(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/global_accounts":
			w.Write([]byte(globalAccountsList))
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/simulation/deposit/create":
			deposit = decodeNumbers(t, readBody(t, r))
			w.Write([]byte(`{}`))
		default:
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
	})
	amount, err := domain.NewMoney(100000, domain.USD)
	if err != nil {
		t.Fatal(err)
	}

	if err := s.FundAccount(t.Context(), amount); err != nil {
		t.Fatalf("FundAccount: %v", err)
	}

	if deposit["global_account_id"] != "ga_usd" {
		t.Errorf("global_account_id = %v, want ga_usd (the account whose required_features are USD)", deposit["global_account_id"])
	}
	// The docs type amount as a number, in major units.
	if got, ok := deposit["amount"].(json.Number); !ok || got.String() != "1000.00" {
		t.Errorf("amount = %#v, want the JSON number 1000.00", deposit["amount"])
	}
}

func TestSimulator_FundAccount_CreatesAccountWhenNoneMatches(t *testing.T) {
	var created, deposit map[string]any
	s := newTestSimulator(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/global_accounts":
			w.Write([]byte(`{"items":[{"id":"ga_hkd","country_code":"HK","required_features":[{"currency":"HKD"}]}]}`))
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/global_accounts/create":
			created = decodeNumbers(t, readBody(t, r))
			w.WriteHeader(http.StatusCreated)
			w.Write([]byte(`{"id":"ga_new"}`))
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/simulation/deposit/create":
			deposit = decodeNumbers(t, readBody(t, r))
			w.Write([]byte(`{}`))
		default:
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
	})
	amount, err := domain.NewMoney(100000, domain.USD)
	if err != nil {
		t.Fatal(err)
	}

	if err := s.FundAccount(t.Context(), amount); err != nil {
		t.Fatalf("FundAccount: %v", err)
	}

	// Documented required fields: country_code, request_id,
	// required_features[].currency. A fixed request_id per currency
	// means a retried create is recognised, not duplicated.
	want := map[string]any{
		"country_code": "US",
		"request_id":   "enquoir-global-account-USD",
		"nick_name":    "Enquoir USD",
		"required_features": []any{
			map[string]any{"currency": "USD", "transfer_method": "LOCAL"},
		},
	}
	if !reflect.DeepEqual(created, want) {
		t.Errorf("create body = %#v\nwant %#v", created, want)
	}
	if deposit["global_account_id"] != "ga_new" {
		t.Errorf("deposit went to %v, want the new account ga_new", deposit["global_account_id"])
	}
}
