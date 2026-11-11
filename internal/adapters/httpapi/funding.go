package httpapi

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"enquoir/internal/app"
	"enquoir/internal/commerce"
	"enquoir/internal/commerce/commerceapp"
)

// FundingUseCases are the funding-method binding use cases.
type FundingUseCases struct {
	Register *commerceapp.RegisterFundingMethod
	Get      *commerceapp.GetFundingMethod
	List     *commerceapp.ListFundingMethods
	Status   *commerceapp.SetFundingMethodStatus
}

type fundingDisplayDTO struct {
	Brand    string  `json:"brand"`
	LastFour *string `json:"last_four"`
}

type fundingMethodDTO struct {
	ID         string            `json:"id"`
	CustomerID string            `json:"customer_id"`
	Reference  string            `json:"merchant_funding_method_id"`
	Provider   string            `json:"provider"`
	Display    fundingDisplayDTO `json:"display"`
	Status     string            `json:"status"`
	CreatedAt  time.Time         `json:"created_at"`
	DisabledAt *time.Time        `json:"disabled_at"`
}

func newFundingMethodDTO(f commerce.FundingMethod) fundingMethodDTO {
	return fundingMethodDTO{
		ID: string(f.ID), CustomerID: string(f.CustomerID), Reference: string(f.Reference), Provider: string(f.Provider),
		Display: fundingDisplayDTO{Brand: f.Display.Brand, LastFour: optional(f.Display.LastFour)},
		Status:  string(f.Status), CreatedAt: f.CreatedAt.UTC(), DisabledAt: utcPtr(f.DisabledAt),
	}
}

// decodeStrict decodes data into v, refusing any field v doesn't
// declare. The error names the field (prefixed with path) but never its
// value. Used where an unexpected field may be card data: it must be
// refused, not silently dropped.
func decodeStrict(data []byte, v any, path string) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	err := dec.Decode(v)
	if err == nil {
		if dec.More() {
			return errNotJSON
		}
		return nil
	}
	if name, ok := strings.CutPrefix(err.Error(), "json: unknown field "); ok {
		field := path + strings.Trim(name, `"`)
		return &app.ValidationError{Field: field, Message: fmt.Sprintf("unknown field %s: send only the documented fields - never card data, CVVs, bank credentials or PSP tokens", field)}
	}
	var typeErr *json.UnmarshalTypeError
	if errors.As(err, &typeErr) {
		field := path + typeErr.Field
		return &app.ValidationError{Field: field, Message: field + " has the wrong type"}
	}
	return errNotJSON
}

var errNotJSON = errors.New("not JSON")

func (h *Handler) registerFundingMethod(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	var tooLarge *http.MaxBytesError
	if errors.As(err, &tooLarge) {
		writeProblem(w, http.StatusRequestEntityTooLarge, codeBodyTooLarge, fmt.Sprintf("the request body is over the %d-byte limit", MaxRequestBodyBytes))
		return
	}
	var req struct {
		CustomerID string          `json:"customer_id"`
		Provider   string          `json:"provider"`
		Reference  string          `json:"merchant_funding_method_id"`
		Display    json.RawMessage `json:"display"`
	}
	var display struct {
		Brand    string `json:"brand"`
		LastFour string `json:"last_four"`
	}
	if err == nil {
		err = decodeStrict(body, &req, "")
	}
	if err == nil && len(req.Display) > 0 && string(req.Display) != "null" {
		err = decodeStrict(req.Display, &display, "display.")
	}
	if err != nil {
		if errors.Is(err, errNotJSON) || !errors.As(err, new(*app.ValidationError)) {
			writeProblem(w, http.StatusBadRequest, codeInvalidJSON, "the request body is not valid JSON")
			return
		}
		writeAppError(w, err, "")
		return
	}
	fm, created, err := h.Funding.Register.Run(r.Context(), merchantFrom(r.Context()), commerceapp.FundingMethodCommand{
		CustomerID: commerce.CustomerID(req.CustomerID), Reference: req.Reference, Provider: req.Provider,
		Brand: display.Brand, LastFour: display.LastFour,
	})
	if err != nil {
		writeAppError(w, err, "could not register the funding method")
		return
	}
	writeJSON(w, registeredStatus(created), newFundingMethodDTO(fm))
}

func (h *Handler) getFundingMethod(w http.ResponseWriter, r *http.Request) {
	fm, err := h.Funding.Get.Run(r.Context(), merchantFrom(r.Context()), commerce.FundingMethodID(chi.URLParam(r, "id")))
	if err != nil {
		writeAppError(w, err, "could not load the funding method")
		return
	}
	writeJSON(w, http.StatusOK, newFundingMethodDTO(fm))
}

func (h *Handler) listCustomerFundingMethods(w http.ResponseWriter, r *http.Request) {
	q := &query{v: r.URL.Query()}
	f := commerceapp.ListFilter{Status: commerce.Status(q.str("status")), Limit: q.int("limit"), Cursor: q.str("cursor")}
	if q.err != nil {
		writeAppError(w, q.err, "")
		return
	}
	page, err := h.Funding.List.Run(r.Context(), merchantFrom(r.Context()), commerce.CustomerID(chi.URLParam(r, "id")), f)
	if err != nil {
		writeAppError(w, err, "could not list funding methods")
		return
	}
	out := make([]fundingMethodDTO, len(page.Methods))
	for i, m := range page.Methods {
		out[i] = newFundingMethodDTO(m)
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": out, "next_cursor": optional(page.NextCursor)})
}

func (h *Handler) setFundingMethodStatus(status commerce.Status) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		fm, err := h.Funding.Status.Run(r.Context(), merchantFrom(r.Context()), commerce.FundingMethodID(chi.URLParam(r, "id")), status)
		if err != nil {
			writeAppError(w, err, "could not change the funding method")
			return
		}
		writeJSON(w, http.StatusOK, newFundingMethodDTO(fm))
	}
}
