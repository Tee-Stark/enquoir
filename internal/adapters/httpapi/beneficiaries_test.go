package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"enquoir/internal/app"
	"enquoir/internal/domain"
)

type fakeRegistrar struct {
	mu    sync.Mutex
	calls []domain.BeneficiaryKey
	err   error
}

func (r *fakeRegistrar) Register(ctx context.Context, b domain.Beneficiary) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, b.Key)
	if r.err != nil {
		return "", r.err
	}
	return "ben_" + string(b.Key)[:8], nil
}

func newBeneficiaryRouter(registrar *fakeRegistrar) http.Handler {
	return NewRouter(&Handler{Register: &app.RegisterBeneficiary{Registrar: registrar}})
}

func postBeneficiary(t *testing.T, router http.Handler, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/beneficiaries", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	return rec
}

const validBeneficiaryBody = `{
	"name": "Acme Ltd",
	"account_number": "50001121",
	"routing_number": "021000021",
	"account_category": "Checking",
	"address": {"street": "412 5th Avenue", "city": "Seattle", "state": "WA", "postcode": "98104"}
}`

type registerResponse struct {
	ID    string `json:"id"`
	Key   string `json:"key"`
	Error string `json:"error"`
}

func decodeRegister(t *testing.T, rec *httptest.ResponseRecorder) registerResponse {
	t.Helper()
	var resp registerResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decoding response %s: %v", rec.Body, err)
	}
	return resp
}

func TestRegisterBeneficiary_ValidRequestReturnsIDAndKey(t *testing.T) {
	registrar := &fakeRegistrar{}
	rec := postBeneficiary(t, newBeneficiaryRouter(registrar), validBeneficiaryBody)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body %s", rec.Code, rec.Body)
	}
	resp := decodeRegister(t, rec)
	if len(resp.Key) != 32 {
		t.Errorf("key = %q, want a 32-character beneficiary key", resp.Key)
	}
	if resp.ID != "ben_"+resp.Key[:8] {
		t.Errorf("id = %q, want the registrar's id for key %q", resp.ID, resp.Key)
	}
}

func TestRegisterBeneficiary_SameDetailsTwiceReturnSameKey(t *testing.T) {
	registrar := &fakeRegistrar{}
	router := newBeneficiaryRouter(registrar)

	first := decodeRegister(t, postBeneficiary(t, router, validBeneficiaryBody))
	second := decodeRegister(t, postBeneficiary(t, router, strings.Replace(validBeneficiaryBody, `"Acme Ltd"`, `"  Acme Ltd "`, 1)))

	if first.Key != second.Key || first.ID != second.ID {
		t.Errorf("same details gave different results: %+v vs %+v", first, second)
	}
}

func TestRegisterBeneficiary_RejectsInvalidInputWithoutCallingRegistrar(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{"malformed JSON", `{"name":`},
		{"missing name", strings.Replace(validBeneficiaryBody, `"Acme Ltd"`, `""`, 1)},
		{"bad routing checksum", strings.Replace(validBeneficiaryBody, `"021000021"`, `"021000022"`, 1)},
		{"bad account number", strings.Replace(validBeneficiaryBody, `"50001121"`, `"50-01121"`, 1)},
		{"missing address", `{"name":"Acme Ltd","account_number":"50001121","routing_number":"021000021","account_category":"Checking"}`},
		{"missing account category", strings.Replace(validBeneficiaryBody, `"account_category": "Checking",`, ``, 1)},
		{"state spelled out", strings.Replace(validBeneficiaryBody, `"state": "WA"`, `"state": "Washington"`, 1)},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			registrar := &fakeRegistrar{}
			rec := postBeneficiary(t, newBeneficiaryRouter(registrar), tt.body)
			if rec.Code != http.StatusBadRequest {
				t.Errorf("status = %d, want 400; body %s", rec.Code, rec.Body)
			}
			if len(registrar.calls) != 0 {
				t.Errorf("registrar called %d times for invalid input", len(registrar.calls))
			}
		})
	}
}

func TestRegisterBeneficiary_MapsRegistrarErrors(t *testing.T) {
	tests := []struct {
		name       string
		err        error
		wantStatus int
		wantInBody string
	}{
		{"rail rejection is 422 with the rail's reason", fmt.Errorf("validation_failed: postcode: %w", app.ErrRejected), http.StatusUnprocessableEntity, "postcode"},
		{"anything else is 500", errors.New("temporal unavailable"), http.StatusInternalServerError, "could not register beneficiary"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := postBeneficiary(t, newBeneficiaryRouter(&fakeRegistrar{err: tt.err}), validBeneficiaryBody)
			if rec.Code != tt.wantStatus {
				t.Errorf("status = %d, want %d; body %s", rec.Code, tt.wantStatus, rec.Body)
			}
			if resp := decodeRegister(t, rec); !strings.Contains(resp.Error, tt.wantInBody) {
				t.Errorf("error %q does not mention %q", resp.Error, tt.wantInBody)
			}
		})
	}
}
