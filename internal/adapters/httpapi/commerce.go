package httpapi

import (
	"context"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"

	"enquoir/internal/app"
	"enquoir/internal/commerce"
	"enquoir/internal/commerce/commerceapp"
)

// CommerceUseCases are the merchant-delegated commerce use cases: the
// dashboard's merchant integrations, and the merchant's own routes under
// /v1/commerce.
type CommerceUseCases struct {
	CreateIntegration    *commerceapp.CreateIntegration
	ListIntegrations     *commerceapp.ListIntegrations
	GetIntegration       *commerceapp.GetIntegration
	SetIntegrationStatus *commerceapp.SetIntegrationStatus
	RotateIntegrationKey *commerceapp.RotateIntegrationKey
	RevokeIntegrationKey *commerceapp.RevokeIntegrationKey
	AuthenticateMerchant *commerceapp.AuthenticateMerchant

	RegisterCustomer  *commerceapp.RegisterCustomer
	ListCustomers     *commerceapp.ListCustomers
	GetCustomer       *commerceapp.GetCustomer
	SetCustomerStatus *commerceapp.SetCustomerStatus

	RegisterAgent  *commerceapp.RegisterAgent
	ListAgents     *commerceapp.ListAgents
	GetAgent       *commerceapp.GetAgent
	SetAgentStatus *commerceapp.SetAgentStatus
}

func merchantFrom(ctx context.Context) commerceapp.MerchantPrincipal {
	p, _ := ctx.Value(merchantKey).(commerceapp.MerchantPrincipal)
	return p
}

// requireMerchant admits only requests carrying a valid merchant
// integration key, as "Authorization: Bearer <key>". Nothing else is a
// merchant credential: not a treasury agent key, not a dashboard
// session. Every commerce record is scoped by the merchant it resolves
// to - never by a merchant named in the request.
func (h *Handler) requireMerchant(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p, err := h.Commerce.AuthenticateMerchant.Run(r.Context(), bearerToken(r))
		if err != nil {
			writeAppError(w, err, "could not check the integration key")
			return
		}
		ctx := context.WithValue(r.Context(), merchantKey, p)
		actor := app.ActorFrom(ctx)
		actor.MerchantID, actor.MerchantName = string(p.MerchantID), p.MerchantName
		next.ServeHTTP(w, r.WithContext(app.WithActor(ctx, actor)))
	})
}

type integrationKeyDTO struct {
	ID         string     `json:"id"`
	Prefix     string     `json:"prefix"`
	CreatedAt  time.Time  `json:"created_at"`
	LastUsedAt *time.Time `json:"last_used_at"`
}

type integrationDTO struct {
	ID         string             `json:"id"`
	Name       string             `json:"name"`
	Status     string             `json:"status"`
	CreatedAt  time.Time          `json:"created_at"`
	DisabledAt *time.Time         `json:"disabled_at"`
	Key        *integrationKeyDTO `json:"key"`
}

type issuedIntegrationDTO struct {
	Integration integrationDTO `json:"integration"`
	Key         issuedKeyDTO   `json:"key"`
}

func newIntegrationDTO(i commerceapp.Integration) integrationDTO {
	d := integrationDTO{ID: string(i.ID), Name: i.Name, Status: string(i.Status), CreatedAt: i.CreatedAt.UTC(), DisabledAt: utcPtr(i.DisabledAt)}
	if i.Key != nil {
		d.Key = &integrationKeyDTO{ID: i.Key.ID, Prefix: i.Key.Prefix, CreatedAt: i.Key.CreatedAt.UTC(), LastUsedAt: utcPtr(i.Key.LastUsedAt)}
	}
	return d
}

func newIssuedIntegrationDTO(i commerceapp.IssuedIntegration) issuedIntegrationDTO {
	k := i.Integration.Key
	return issuedIntegrationDTO{
		Integration: newIntegrationDTO(i.Integration),
		Key:         issuedKeyDTO{ID: k.ID, Prefix: k.Prefix, Secret: i.Secret, CreatedAt: k.CreatedAt.UTC()},
	}
}

func integrationIDParam(r *http.Request) commerce.MerchantID {
	return commerce.MerchantID(chi.URLParam(r, "id"))
}

func (h *Handler) createIntegration(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name string `json:"name"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	issued, err := h.Commerce.CreateIntegration.Run(r.Context(), userFrom(r.Context()).Organisation.ID, req.Name)
	if err != nil {
		writeAppError(w, err, "could not create the merchant integration")
		return
	}
	writeJSON(w, http.StatusCreated, newIssuedIntegrationDTO(issued))
}

func (h *Handler) listIntegrations(w http.ResponseWriter, r *http.Request) {
	list, err := h.Commerce.ListIntegrations.Run(r.Context(), userFrom(r.Context()).Organisation.ID)
	if err != nil {
		writeAppError(w, err, "could not list merchant integrations")
		return
	}
	out := make([]integrationDTO, len(list))
	for i, m := range list {
		out[i] = newIntegrationDTO(m)
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": out})
}

func (h *Handler) getIntegration(w http.ResponseWriter, r *http.Request) {
	m, err := h.Commerce.GetIntegration.Run(r.Context(), userFrom(r.Context()).Organisation.ID, integrationIDParam(r))
	if err != nil {
		writeAppError(w, err, "could not load the merchant integration")
		return
	}
	writeJSON(w, http.StatusOK, newIntegrationDTO(m))
}

func (h *Handler) setIntegrationStatus(status commerce.Status) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		m, err := h.Commerce.SetIntegrationStatus.Run(r.Context(), userFrom(r.Context()).Organisation.ID, integrationIDParam(r), status)
		if err != nil {
			writeAppError(w, err, "could not change the merchant integration")
			return
		}
		writeJSON(w, http.StatusOK, newIntegrationDTO(m))
	}
}

func (h *Handler) rotateIntegrationKey(w http.ResponseWriter, r *http.Request) {
	issued, err := h.Commerce.RotateIntegrationKey.Run(r.Context(), userFrom(r.Context()).Organisation.ID, integrationIDParam(r))
	if err != nil {
		writeAppError(w, err, "could not issue a key")
		return
	}
	writeJSON(w, http.StatusOK, newIssuedIntegrationDTO(issued))
}

func (h *Handler) revokeIntegrationKey(w http.ResponseWriter, r *http.Request) {
	if err := h.Commerce.RevokeIntegrationKey.Run(r.Context(), userFrom(r.Context()).Organisation.ID, integrationIDParam(r)); err != nil {
		writeAppError(w, err, "could not revoke the key")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

type customerDTO struct {
	ID         string     `json:"id"`
	ExternalID string     `json:"external_id"`
	Email      string     `json:"email"`
	Status     string     `json:"status"`
	CreatedAt  time.Time  `json:"created_at"`
	DisabledAt *time.Time `json:"disabled_at"`
}

func newCustomerDTO(c commerce.Customer) customerDTO {
	return customerDTO{ID: string(c.ID), ExternalID: string(c.ExternalID), Email: string(c.Email), Status: string(c.Status),
		CreatedAt: c.CreatedAt.UTC(), DisabledAt: utcPtr(c.DisabledAt)}
}

type commerceAgentDTO struct {
	ID         string     `json:"id"`
	ExternalID string     `json:"external_id"`
	Name       string     `json:"name"`
	Status     string     `json:"status"`
	CreatedAt  time.Time  `json:"created_at"`
	DisabledAt *time.Time `json:"disabled_at"`
}

func newCommerceAgentDTO(a commerce.Agent) commerceAgentDTO {
	return commerceAgentDTO{ID: string(a.ID), ExternalID: string(a.ExternalID), Name: a.Name, Status: string(a.Status),
		CreatedAt: a.CreatedAt.UTC(), DisabledAt: utcPtr(a.DisabledAt)}
}

// registeredStatus is 201 for a new record, 200 for one resolved.
func registeredStatus(created bool) int {
	if created {
		return http.StatusCreated
	}
	return http.StatusOK
}

func listFilter(r *http.Request) (commerceapp.ListFilter, error) {
	q := &query{v: r.URL.Query()}
	f := commerceapp.ListFilter{
		ExternalID: commerce.ExternalID(q.str("external_id")), Status: commerce.Status(q.str("status")),
		Limit: q.int("limit"), Cursor: q.str("cursor"),
	}
	if q.err != nil {
		return f, q.err
	}
	return f, nil
}

func (h *Handler) registerCustomer(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ExternalID string `json:"external_id"`
		Email      string `json:"email"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	c, created, err := h.Commerce.RegisterCustomer.Run(r.Context(), merchantFrom(r.Context()), req.ExternalID, req.Email)
	if err != nil {
		writeAppError(w, err, "could not register the customer")
		return
	}
	writeJSON(w, registeredStatus(created), newCustomerDTO(c))
}

func (h *Handler) listCustomers(w http.ResponseWriter, r *http.Request) {
	f, err := listFilter(r)
	if err != nil {
		writeAppError(w, err, "")
		return
	}
	page, err := h.Commerce.ListCustomers.Run(r.Context(), merchantFrom(r.Context()), f)
	if err != nil {
		writeAppError(w, err, "could not list customers")
		return
	}
	out := make([]customerDTO, len(page.Customers))
	for i, c := range page.Customers {
		out[i] = newCustomerDTO(c)
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": out, "next_cursor": optional(page.NextCursor)})
}

func (h *Handler) getCustomer(w http.ResponseWriter, r *http.Request) {
	c, err := h.Commerce.GetCustomer.Run(r.Context(), merchantFrom(r.Context()), commerce.CustomerID(chi.URLParam(r, "id")))
	if err != nil {
		writeAppError(w, err, "could not load the customer")
		return
	}
	writeJSON(w, http.StatusOK, newCustomerDTO(c))
}

func (h *Handler) setCustomerStatus(status commerce.Status) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		c, err := h.Commerce.SetCustomerStatus.Run(r.Context(), merchantFrom(r.Context()), commerce.CustomerID(chi.URLParam(r, "id")), status)
		if err != nil {
			writeAppError(w, err, "could not change the customer")
			return
		}
		writeJSON(w, http.StatusOK, newCustomerDTO(c))
	}
}

func (h *Handler) registerCommerceAgent(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ExternalID string `json:"external_id"`
		Name       string `json:"name"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	a, created, err := h.Commerce.RegisterAgent.Run(r.Context(), merchantFrom(r.Context()), req.ExternalID, req.Name)
	if err != nil {
		writeAppError(w, err, "could not register the agent")
		return
	}
	writeJSON(w, registeredStatus(created), newCommerceAgentDTO(a))
}

func (h *Handler) listCommerceAgents(w http.ResponseWriter, r *http.Request) {
	f, err := listFilter(r)
	if err != nil {
		writeAppError(w, err, "")
		return
	}
	page, err := h.Commerce.ListAgents.Run(r.Context(), merchantFrom(r.Context()), f)
	if err != nil {
		writeAppError(w, err, "could not list agents")
		return
	}
	out := make([]commerceAgentDTO, len(page.Agents))
	for i, a := range page.Agents {
		out[i] = newCommerceAgentDTO(a)
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": out, "next_cursor": optional(page.NextCursor)})
}

func (h *Handler) getCommerceAgent(w http.ResponseWriter, r *http.Request) {
	a, err := h.Commerce.GetAgent.Run(r.Context(), merchantFrom(r.Context()), commerce.AgentID(chi.URLParam(r, "id")))
	if err != nil {
		writeAppError(w, err, "could not load the agent")
		return
	}
	writeJSON(w, http.StatusOK, newCommerceAgentDTO(a))
}

func (h *Handler) setCommerceAgentStatus(status commerce.Status) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		a, err := h.Commerce.SetAgentStatus.Run(r.Context(), merchantFrom(r.Context()), commerce.AgentID(chi.URLParam(r, "id")), status)
		if err != nil {
			writeAppError(w, err, "could not change the agent")
			return
		}
		writeJSON(w, http.StatusOK, newCommerceAgentDTO(a))
	}
}
