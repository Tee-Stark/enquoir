# Enquoir: project brief

Enquoir is the safe control and observability layer for agentic finance.

AI agents are starting to move money through payment APIs. Models are
probabilistic: they retry, re-plan, lose context, and mint a fresh request ID
for a payment they already sent. Enquoir sits between an agent and the payment
rail and takes the money-safety decisions out of the model.

Three principles drive every design choice:

1. Every action has a policy. Code decides whether a payment may run.
2. Every execution has a history. Each step is recorded before it acts.
3. Every outcome can be investigated. Any payment can be traced and replayed.

The agent only states an intent ("pay this payee this amount for this
invoice"). Enquoir derives a stable key from that intent, checks policy,
executes through a durable workflow, records every transition, and reconciles
unknown outcomes before anything is retried.

The first rail is Airwallex, sandbox only. This is also a hackathon entry with
a submission deadline, so favour a small working slice over breadth.

Enquoir ships as **Enquoir Cloud** (hosted, multi-tenant; the priority) and
later as a self-hosted edition. Cloud consumers sign up, create agents, give
each agent an API key, and watch every payment those agents attempt.

Enquoir has two bounded contexts over shared infrastructure (organisations,
members, authentication, audit, event history, Temporal):

- **Treasury** (built): organisation-funded payments. An organisation's agent
  states an intent and Enquoir itself pays through Airwallex.
- **Merchant-delegated commerce** (in progress, part of the hackathon
  submission): customer-funded purchases. A merchant's AI shopping agent
  proposes a checkout; Enquoir decides whether the customer's mandate allows
  that exact checkout and issues a one-time grant; the merchant charges
  through its own PSP and reports the outcome. Enquoir never holds customer
  funds, card data or PSP secrets. The design is the "Merchant-Delegated
  Commerce" architecture document (v0.1); its roadmap has eight phases,
  tagged `commerce-phase-N`.

## Stack (decided; ask before changing)

- Go, current stable release
- chi for routing, on the standard net/http server
- Temporal Go SDK for durable workflows
- PostgreSQL, accessed with pgx
- Docker Compose for the whole dev environment
- Structured logging with log/slog

Keep dependencies minimal. Approved additions: `github.com/getkin/kin-openapi`,
test-only, to validate the OpenAPI spec and responses against it;
`github.com/redis/go-redis/v9`, for rate limits and the session/API-key cache.
Passwords are hashed with the standard library (`crypto/pbkdf2`), behind a port.
Email goes through Resend's HTTP API with the standard library, no SDK.

Redis is a cache and a rate limiter, never a record: sessions, API keys,
one-time tokens and everything auditable live in Postgres. If Redis is down,
authentication falls back to Postgres and keeps working.

## Architecture: domain-driven design, clean architecture

One Go module. Dependencies point inward only: adapters depend on the
application layer, which depends on the domain. The domain imports nothing
outside the standard library.

    api/              openapi.yaml, the API contract (embedded and served)
    cmd/api/          HTTP server (chi)
    cmd/worker/       Temporal worker
    internal/domain/  Intent, Money, IntentKey, state machine, policy types
    internal/app/     Use cases and ports (IntentRepository, EventLog, Rail, Clock)
    internal/commerce/  Commerce domain: merchants, customers, mandates...
                        (standard library and internal/domain only)
    internal/commerce/commerceapp/  Commerce use cases and ports
    internal/workflow/  Temporal workflows and activities
    internal/adapters/airwallex/  Rail implementation over REST
    internal/adapters/postgres/   Repositories, embedded migrations
    internal/adapters/httpapi/    chi handlers
    deploy/           docker-compose.yml, Dockerfiles
    docs/             progress.md, rail-notes.md
    Makefile, .env.example, README.md

## Safety rules, and why

- **Sandbox only.** Refuse to start if the Airwallex base URL is not
  https://api.sandbox.airwallex.com. Real money must be impossible.
- **Secrets come from the environment.** Read AWX_CLIENT_ID and AWX_API_KEY
  from .env. Never print, log or commit them.
- **No floats for money.** Store integer minor units plus a currency code.
  Airwallex takes major units (100 means one hundred dollars), so convert at
  the adapter boundary only. Support USD, EUR and GBP for now.
- **Workflows are deterministic.** No clock, randomness, network or database
  calls in workflow code. Those belong in activities.
- **Write before you call.** An event is durably recorded before any outbound
  request leaves. This is the core guarantee.
- **No blind retries.** If a call's outcome is unclear, look the transfer up
  by its request ID first. Retry only when it is confirmed absent.
- **Do not guess Airwallex fields.** Use the Airwallex docs MCP server at
  https://mcp.sandbox.airwallex.com/docs (no login) or the API reference.
- **Sandbox simulation calls sit behind their own interface**, separate from
  the Rail port.
- **Tenants are isolated.** Every intent, beneficiary, agent and attempt
  belongs to one organisation. Reads and writes are always scoped by it; a
  key from another organisation is "not found", never "forbidden". The
  intent key includes the organisation, so deduplication is per
  organisation, never across them.
- **Credentials are stored hashed.** Passwords (slow, salted hash), session
  tokens and API keys (SHA-256 of a high-entropy secret). Plaintext secrets
  are shown once, at creation, and never logged.
- **Secrets never rest in plaintext.** One-time link tokens (verification,
  invitations, password reset) are minted when the email is sent and stored
  only as hashes; the email outbox stores a template ID and non-secret values,
  never a link. Templates are embedded in the binary (`go:embed`), not stored.
- **The OpenAPI spec is the contract.** Every route is in api/openapi.yaml
  and every documented route exists; a test enforces both, and handler
  tests validate responses against the spec. Update the spec with the route.

Commerce adds these:

- **No card data, ever.** Enquoir stores its own opaque funding-method
  binding IDs; the merchant keeps the mapping to PSP tokens. No PAN, CVV,
  bank credentials or PSP secrets enter Enquoir, its logs or its events.
- **Three authorities stay separate.** A merchant integration key proves
  which merchant is calling; an agent is an identity the merchant names; only
  the customer grants spending authority, through Enquoir's own consent flow.
  No merchant or agent credential is ever accepted as customer consent.
- **The merchant comes from the credential.** Every commerce resource
  belongs to one merchant integration (and its organisation); the merchant is
  taken from the integration key, never from a request body. Another
  merchant's record is "not found". External IDs are unique per merchant.
- **The merchant backend is the only machine caller** for now. Agents have
  no credentials of their own; checkouts come from the merchant's server,
  never from prices an agent asserts.
- **Treasury and commerce keys are not interchangeable.** An agent key
  (`enq_sk_`) is refused on commerce routes and a merchant key (`enq_mk_`)
  on treasury routes.
- **Commerce outcomes are the merchant's word.** Enquoir checks a reported
  outcome against its grant (amount, currency, references) but cannot
  verify it with the PSP; documentation must say so.

## Test-driven development

Work in red, green, refactor cycles, one behaviour at a time:

1. Write one failing test that describes the next behaviour.
2. Run it and confirm it fails for the expected reason.
3. Write the least code that makes it pass.
4. Run the whole suite. Refactor only while it is green.

Further rules:

- Never weaken, skip or delete a test to get to green. If a test is wrong,
  say so and fix it in its own commit.
- `make test` must pass offline, with no network and no sandbox keys. Tests
  that need Postgres or the real sandbox live behind separate targets.
- Prefer table-driven tests. Use fakes for ports, not mocks of internals.
- Scaffolding, configuration and exploratory probes are exempt from
  test-first. Say when you use this exemption.

## Git

- The remote is GitHub, `Tee-Stark/enquoir` (`origin`). Work on a feature
  branch from an up-to-date `main`, push the branch, and open a pull request
  into `main` with a short bullet-point description. Never push to `main`
  directly; `main` changes only by merging a reviewed pull request.
- Commit after each green cycle. One behaviour per commit, tests and code
  together, with a conventional message such as `feat(domain): derive intent key`.
- Every commit must pass `make test`. Never commit .env.
- Do not amend, rebase or force-push anything. History is a record of the work.
- At the end of a phase, update docs/progress.md, commit, tag the commit
  `phase-N` (commerce phases: `commerce-phase-N`, so the two never clash),
  and open the phase's pull request.

## Email facts

- Enquoir sends through Resend from `Enquoir <no-reply@enquoir.etimi.me>`
  (`EMAIL_FROM`). The domain `enquoir.etimi.me` is verified in Resend, so
  email reaches any address; the frontend, and every emailed link, live at
  `APP_BASE_URL` (`https://enquoir.etimi.me`).
- `RESEND_API_KEY` is a secret: from the environment only, never printed,
  logged or committed.
- Automated tests send only to Resend's test inboxes
  (`delivered+label@resend.dev`), never to real people.

## Airwallex facts to build on

From the hackathon builder guide (https://airwallexdev.com/guide). Verify
each one you depend on, and record differences in docs/rail-notes.md.

- Base URL https://api.sandbox.airwallex.com, paths under /api/v1.
- Auth: POST /api/v1/authentication/login with headers x-client-id and
  x-api-key, no body. Returns a bearer token valid for 30 minutes.
- Mutating calls take a request_id. Reusing one for a different operation
  within 7 days returns duplicate_request_id. Transfer request IDs must be 10
  to 50 characters.
- Create a transfer with POST /transfers/create, sending transfer_currency and
  transfer_amount. transfer_method is LOCAL or SWIFT. Use the US, USD, LOCAL
  corridor. (The guide says LOCAL payouts have no fee; the sandbox charged
  one - see docs/rail-notes.md.)
- Read the beneficiary form schema before building a beneficiary payload.
  (The guide says POST /beneficiaries/schema; the live endpoint is
  POST /beneficiary_form_schemas/generate - see docs/rail-notes.md.)
- Fund the sandbox: list or create a Global Account, then
  POST /simulation/deposit/create.
- Move a transfer through states with
  POST /simulation/transfers/{id}/transition. Simulate SENT before PAID.
- SENT means in flight, not final. A failed transfer ends as CANCELLED, never
  FAILED. Read failure_type and failure_reason.
- Limits: 20 requests per second overall, 10 per endpoint, 10 concurrent.

## Commerce Phase 1: merchant integration and identity (current)

Done when a merchant can securely connect, and register its agents and
customers.

- Merchant integrations, managed from the dashboard: create, list, get,
  disable, enable. An organisation may have several (one per storefront).
  Managing them needs admin or above with a verified email; any member may
  read them.
- One integration key each (`enq_mk_`), shown once; rotate and revoke. A
  disabled integration's key stops working at once, cache included.
- Merchant routes under `/v1/commerce`, integration key only: register or
  resolve customers (by the merchant's external ID, with a contact email for
  later consent links) and agents (by external ID, with a name); list, get,
  disable and enable them. Repeating a registration returns the same record.
- Audit events for integration, key, customer and agent changes.
- PoC currencies, when money arrives in later phases: USD, EUR and GBP.

Commerce phases 2 to 8 (funding-method bindings, consent and mandates,
checkout evaluation, grants, execution reporting, reconciliation, SDK and
demo) come after, one at a time. The spending-policy engine and escalation
to humans follow commerce and are wired in alongside commerce mandates.

## Phase 5: authentication hardening (done)

- Organisation members with roles: owner, admin, operator (read and write),
  viewer (read only). One person may belong to several organisations; a
  session is bound to one. Nobody grants a role above their own; an
  organisation always keeps an owner; a role change or removal signs the
  member out of that organisation at once.
- Invitations by email; accepting adds the membership (or creates the
  account). Only verified accounts may create agents, invite, or register
  payees, so only an organisation's own agents can create its intents.
- Email verification, password reset and change, through Resend, with
  embedded templates and a durable send. Links point at `APP_BASE_URL`.
- Rate limits on log-in, sign-up, verification resends and resets; an
  account locks for an hour after 10 failed log-ins.
- Session listing and revocation; an audit log of account actions.

## Phase 4: Enquoir Cloud (done)

- An OpenAPI 3.1 spec for every endpoint, kept current with each addition.
- Accounts: sign up, log in and log out with email and password; cookie
  sessions for the dashboard. Email verification and onboarding are
  placeholders (documented, 501) until their requirements arrive.
- Agents, one API key each; agents authenticate with their key.
- Attempts: every agent submission and every Airwallex call is recorded,
  distinguished by kind and filterable.
- History: payments and attempts over a period, per payment detail.
- Replay: every step a payment went through, ordered and timed, for the
  frontend to animate. The backend supplies data; it has no UI.
- Analytics: a summary and time series for charts, in any IANA time zone.
- For now every organisation pays through Enquoir's one sandbox Airwallex
  account; per-organisation rail credentials are a later seam.

## Out of scope until told otherwise

Webhooks, currency conversion, compensation, the full policy engine and
escalation to humans (after commerce), any UI, the MCP proxy, cards,
connected accounts, per-organisation Airwallex credentials, two-factor
authentication, agent credentials for commerce, a real PSP integration
before commerce asks for one, and the self-hosted edition. Leave clear seams
where they will attach.

## How to work

- At the start of a session, read this file and docs/progress.md.
- Do only the phase you are given. Stop and report when it is done.
- Say plainly what you verified and what you did not.
- Ask before changing the stack, these rules or the phase scope. Otherwise
  make reasonable choices and note them in docs/progress.md.
