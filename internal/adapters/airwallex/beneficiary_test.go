package airwallex

import (
	"encoding/json"
	"errors"
	"net/http"
	"reflect"
	"testing"
	"time"

	"enquoir/internal/app"
	"enquoir/internal/domain"
)

func testBeneficiary(t *testing.T) domain.Beneficiary {
	t.Helper()
	b, err := domain.NewBeneficiary("Acme Ltd", "50001121", "021000021", domain.Checking, domain.BeneficiaryAddress{
		Street:   "412 5th Avenue",
		City:     "Seattle",
		State:    "WA",
		Postcode: "98104",
	})
	if err != nil {
		t.Fatalf("building test beneficiary: %v", err)
	}
	return b
}

func TestClient_CreateBeneficiary_SendsDocumentedUSLocalPayload(t *testing.T) {
	b := testBeneficiary(t)
	var got map[string]any
	ts, _ := server(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/v1/beneficiaries/create" {
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		if err := json.Unmarshal(readBody(t, r), &got); err != nil {
			t.Fatalf("unmarshalling request body: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"id":"ben_1","nickname":"` + string(b.Key) + `"}`))
	})
	c := newTestClient(t, ts, &fakeClock{now: time.Now()})

	id, err := c.CreateBeneficiary(t.Context(), b)
	if err != nil {
		t.Fatalf("CreateBeneficiary: %v", err)
	}
	if id != "ben_1" {
		t.Errorf("id = %q, want %q", id, "ben_1")
	}

	want := map[string]any{
		"nickname":         string(b.Key),
		"transfer_methods": []any{"LOCAL"},
		"beneficiary": map[string]any{
			"entity_type":  "COMPANY",
			"company_name": "Acme Ltd",
			"address": map[string]any{
				"street_address": "412 5th Avenue",
				"city":           "Seattle",
				"state":          "US-WA", // ISO 3166-2, per the live schema
				"postcode":       "98104",
				"country_code":   "US",
			},
			"bank_details": map[string]any{
				"account_currency":       "USD",
				"account_name":           "Acme Ltd",
				"account_number":         "50001121",
				"account_routing_type1":  "aba",
				"account_routing_value1": "021000021",
				"bank_account_category":  "Checking",
				"bank_country_code":      "US",
				"local_clearing_system":  "ACH",
			},
		},
	}
	if !reflect.DeepEqual(got, want) {
		gotJSON, _ := json.MarshalIndent(got, "", "  ")
		wantJSON, _ := json.MarshalIndent(want, "", "  ")
		t.Errorf("request body mismatch\ngot:  %s\nwant: %s", gotJSON, wantJSON)
	}
}

func TestClient_CreateBeneficiary_ClassifiesFailures(t *testing.T) {
	tests := []struct {
		name    string
		status  int
		body    string
		wantErr error
	}{
		{"validation failure is a rejection", http.StatusBadRequest, `{"code":"validation_failed","message":"This is not valid."}`, app.ErrRejected},
		{"server error is ambiguous", http.StatusInternalServerError, `{"message":"boom"}`, app.ErrAmbiguous},
		{"2xx without an id is ambiguous", http.StatusCreated, `{}`, app.ErrAmbiguous},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ts, _ := server(t, func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tt.status)
				w.Write([]byte(tt.body))
			})
			c := newTestClient(t, ts, &fakeClock{now: time.Now()})

			_, err := c.CreateBeneficiary(t.Context(), testBeneficiary(t))
			if !errors.Is(err, tt.wantErr) {
				t.Errorf("CreateBeneficiary error = %v, want it to wrap %v", err, tt.wantErr)
			}
		})
	}
}

func TestClient_FindBeneficiary(t *testing.T) {
	key := testBeneficiary(t).Key

	tests := []struct {
		name    string
		status  int
		body    string
		wantID  string
		wantOK  bool
		wantErr error
	}{
		{"exact nickname match is found", http.StatusOK,
			`{"items":[{"id":"ben_1","nickname":"` + string(key) + `"}]}`, "ben_1", true, nil},
		{"no items is not found", http.StatusOK, `{"items":[]}`, "", false, nil},
		// The docs only say the list endpoint "filters on" nickname,
		// not that it matches exactly, so a near match must not count.
		{"near nickname match is not found", http.StatusOK,
			`{"items":[{"id":"ben_2","nickname":"` + string(key) + `-old"}]}`, "", false, nil},
		{"non-200 is ambiguous", http.StatusInternalServerError, `{"message":"boom"}`, "", false, app.ErrAmbiguous},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ts, _ := server(t, func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet || r.URL.Path != "/api/v1/beneficiaries" {
					t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
				}
				if got := r.URL.Query().Get("nick_name"); got != string(key) {
					t.Errorf("nick_name = %q, want %q", got, key)
				}
				w.WriteHeader(tt.status)
				w.Write([]byte(tt.body))
			})
			c := newTestClient(t, ts, &fakeClock{now: time.Now()})

			id, ok, err := c.FindBeneficiary(t.Context(), key)
			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("FindBeneficiary error = %v, want it to wrap %v", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("FindBeneficiary: %v", err)
			}
			if id != tt.wantID || ok != tt.wantOK {
				t.Errorf("FindBeneficiary = (%q, %v), want (%q, %v)", id, ok, tt.wantID, tt.wantOK)
			}
		})
	}
}
