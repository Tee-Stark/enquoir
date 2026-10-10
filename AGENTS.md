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

## Stack (decided; ask before changing)

- Go, current stable release
- chi for routing, on the standard net/http server
- Temporal Go SDK for durable workflows
- PostgreSQL, accessed with pgx
- Docker Compose for the whole dev environment
- Structured logging with log/slog

Keep dependencies minimal.

## Architecture: domain-driven design, clean architecture

One Go module. Dependencies point inward only: adapters depend on the
application layer, which depends on the domain. The domain imports nothing
outside the standard library.

    cmd/api/          HTTP server (chi)
    cmd/worker/       Temporal worker
    internal/domain/  Intent, Money, IntentKey, state machine, policy types
    internal/app/     Use cases and ports (IntentRepository, EventLog, Rail, Clock)
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

- Local repository only. Do not add a remote or push.
- Commit after each green cycle. One behaviour per commit, tests and code
  together, with a conventional message such as `feat(domain): derive intent key`.
- Every commit must pass `make test`. Never commit .env.
- Do not amend, rebase or force anything. History is a record of the work.
- At the end of a phase, update docs/progress.md, commit, and tag the commit
  `phase-1`, `phase-2` or `phase-3`.

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
  corridor, since LOCAL payouts have no fee.
- Call POST /beneficiaries/schema before building a beneficiary payload.
- Fund the sandbox: list or create a Global Account, then
  POST /simulation/deposit/create.
- Move a transfer through states with
  POST /simulation/transfers/{id}/transition. Simulate SENT before PAID.
- SENT means in flight, not final. A failed transfer ends as CANCELLED, never
  FAILED. Read failure_type and failure_reason.
- Limits: 20 requests per second overall, 10 per endpoint, 10 concurrent.

## Out of scope until told otherwise

Webhooks, currency conversion, compensation, the full policy engine, the
replayer, any UI, the MCP proxy, cards, and connected accounts. Leave clear
seams where they will attach.

## How to work

- At the start of a session, read this file and docs/progress.md.
- Do only the phase you are given. Stop and report when it is done.
- Say plainly what you verified and what you did not.
- Ask before changing the stack, these rules or the phase scope. Otherwise
  make reasonable choices and note them in docs/progress.md.
