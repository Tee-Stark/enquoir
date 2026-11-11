# Enquoir

Enquoir is a safe control and observability layer for agentic finance: it
sits between an AI agent and a payment rail, takes the money-safety
decisions out of the model, and makes sure a payment can't be sent twice
even when the agent retries, the worker crashes mid-call, or both happen
at once.

Three rules drive everything here: every action has a policy, every
execution has a history, and every outcome can be investigated. See
[AGENTS.md](AGENTS.md) for the full brief and the reasoning behind the
safety rules, and [docs/progress.md](docs/progress.md) for what's been
built and verified phase by phase.

## Prerequisites

- Go (current stable release)
- Docker Desktop (or another Docker Engine) with Compose
- Airwallex sandbox credentials - a client ID and API key for
  `https://api.sandbox.airwallex.com`

## Setup

```sh
cp .env.example .env
```

Edit `.env` and fill in `AWX_CLIENT_ID` and `AWX_API_KEY`. Everything else
in `.env.example` has a sane default, including ports chosen to dodge
anything else already running on a typical dev machine (Postgres on
`5433` not `5432`, the Temporal UI on `8088` not `8080`, the API on
`8090`) - change them if those collide with you too.

```sh
make up            # Postgres + Temporal + its web UI, via Docker Compose
make sandbox-setup # idempotent: a funded USD Global Account, one test beneficiary
make test          # offline - no network, no sandbox keys, nothing from `make up` needed
```

`make up` must leave all three Compose services healthy before anything
else will work; `docker compose -f deploy/docker-compose.yml ps` shows
their status. The Temporal Web UI is at `http://localhost:8088` (or
whatever `TEMPORAL_UI_PORT` you set).

## Running it

Start a worker (it registers the payment workflow and its activities,
wired to the real Postgres repositories and the real Airwallex sandbox):

```sh
make worker
```

In another terminal, either run the one-shot demo, or bring up the HTTP
API and drive it yourself.

```sh
make demo   # submits one intent, runs its own in-process worker, pays it
            # through to PAID via the simulator, prints the final state
            # and full event history - self-contained, doesn't need
            # `make worker` running
```

```sh
make api    # listens on :8090 (HTTP_ADDR)
```

Enquoir Cloud is multi-tenant. A person signs up (which creates their
organisation), then creates an agent; the agent pays with its API key.
API reference: `http://localhost:8090/docs` (spec at `/openapi.yaml`).

```sh
curl -c cookies -X POST localhost:8090/v1/auth/signup -H 'Content-Type: application/json' \
  -d '{"email":"you@example.com","password":"correct horse battery","organisation_name":"Acme Treasury"}'

curl -b cookies -X POST localhost:8090/v1/agents -H 'Content-Type: application/json' -d '{"name":"Invoice bot"}'
# {"agent":{...},"api_key":{"secret":"enq_sk_...", ...}}   <- shown once; store it
KEY=enq_sk_...
```

The agent registers the payee first. Airwallex only pays a beneficiary it
already holds, so an intent's `payee` is the `id` this returns, and only
the organisation that registered a payee can pay it:

```sh
curl -X POST localhost:8090/v1/beneficiaries -H "Authorization: Bearer $KEY" \
  -H 'Content-Type: application/json' \
  -d '{"name":"Acme Ltd","account_number":"50001121","routing_number":"021000021","account_category":"Checking",
       "address":{"street":"412 5th Avenue","city":"Seattle","state":"WA","postcode":"98104"}}'
# {"id":"<beneficiary id>","key":"..."}
```

Only US company accounts paid in USD over ACH are supported for now.
Registering the same details again returns the same `id`: Airwallex's
beneficiary create has no idempotency key, so Enquoir stores its own key
as the beneficiary's nickname and looks it up before ever creating one.
The key includes the organisation, so another organisation registering
the same details gets its own beneficiary. A rail rejection comes back as
a 422 carrying Airwallex's reason.

```sh
curl -X POST localhost:8090/v1/intents -H "Authorization: Bearer $KEY" \
  -H 'Content-Type: application/json' \
  -d '{"payee":"<beneficiary id>","amount":"10.50","currency":"USD","reference":"INV-1001","reason":"invoice settlement"}'
# {"key":"...","state":"PROPOSED"}

curl localhost:8090/v1/intents/<key> -H "Authorization: Bearer $KEY"
# {"key":"...","state":"APPLIED","events":[...]}

curl localhost:8090/healthz
# ok
```

Errors are RFC 9457 problem details with a stable `code`: for example a
payee the organisation never registered is `422 payee_unknown`, and a
missing or revoked key is `401 unauthenticated`.

The dashboard (signed in, with the session cookie) sees everything the
organisation's agents did:

```sh
curl -b cookies 'localhost:8090/v1/payments?state=APPLIED,FAILED&from=2026-11-01T00:00:00Z'
curl -b cookies localhost:8090/v1/payments/<key>           # events, chain verdict, every attempt
curl -b cookies localhost:8090/v1/payments/<key>/replay    # every step, ordered and timed
curl -b cookies 'localhost:8090/v1/attempts?kind=rail_call&outcome=interrupted,ambiguous'
curl -b cookies 'localhost:8090/v1/analytics/summary?tz=Africa/Lagos'
curl -b cookies 'localhost:8090/v1/analytics/timeseries?metric=payments&interval=day&group_by=state&tz=Africa/Lagos'
```

A frontend on another origin must be listed in `CORS_ALLOWED_ORIGINS`;
`docs/frontend.md` covers what a dashboard needs to know, and
`docs/deployment.md` how to deploy Enquoir on any cloud platform.

### Teams, email and limits

Organisations have members with roles (owner, admin, operator, viewer),
invited by email. Sign-up sends a verification link; until it's followed,
the account can't create agents or invite anyone. Password reset and
change, session listing and revocation, and an audit log are built in.
Log-in, sign-up, resends and resets are rate limited, and 10 failed
log-ins lock an account for an hour.

Email goes through Resend from `Enquoir <no-reply@enquoir.etimi.me>`
(`EMAIL_PROVIDER=resend`, `RESEND_API_KEY`, `EMAIL_FROM`; the domain is
verified, so any address receives it) or to a file
(`EMAIL_PROVIDER=file`, `MAIL_FILE`).
`make emails` writes previews of every template to `email-previews/`.
Redis (`REDIS_URL`, started by `make up`) holds rate-limit counters and a
short cache of sessions and keys; without it everything still works.

Posting the same intent again (same organisation, payee, amount,
currency and reference) returns the same key, reports its recorded state, and starts
nothing new - ever, not just while the first run is going. The `reason`
is recorded but deliberately left out of the key: it is free text an
agent writes, and an agent rewords it when it retries, so a reworded
retry must still be the same payment.
The event log decides, not Temporal: once anything is recorded for a
key, a resubmission never starts another run, since Temporal forgets
finished runs after its retention period and Airwallex forgets a
`request_id` after 7 days. `GET` 404s until a worker has actually
processed at least one activity for that intent; nothing recorded yet is
nothing observed, by design.

If the rail stops answering mid-payment, the intent waits in `UNKNOWN`
(or `IN_FLIGHT`) and keeps retrying its lookup with backoff, capped at 5
minutes, rather than failing - it settles by itself once the rail is
back. The one thing that stops a run is the rail holding two transfers
for one request ID: a double payment, left for a person to investigate.

### Merchant-delegated commerce (in progress)

The second bounded context: a merchant's AI shopping agent proposes
purchases funded by the merchant's customers, and Enquoir decides
whether the customer's authority allows each one. The merchant charges
through its own payment provider; Enquoir never holds customer funds,
card data or PSP secrets. Phases 1 and 2 of 8 are built: merchant
integration and identity, and funding-method bindings.

An owner or admin connects a merchant from the dashboard and gets its
integration key (`enq_mk_...`, shown once). The merchant's backend uses
it on `/v1/commerce` to register its customers (with the email consent
links will later go to) and its agents, by its own external IDs:

```sh
curl -b cookies -X POST localhost:8090/v1/merchant-integrations -d '{"name":"Acme Electronics"}'
# {"integration":{...},"key":{"secret":"enq_mk_...", ...}}   <- shown once; store it

curl -H "Authorization: Bearer $MERCHANT_KEY" -X POST localhost:8090/v1/commerce/customers \
  -d '{"external_id":"cust_42","email":"ada@example.com"}'     # 201, then 200 on a repeat
curl -H "Authorization: Bearer $MERCHANT_KEY" -X POST localhost:8090/v1/commerce/agents \
  -d '{"external_id":"shopping-assistant","name":"Acme Shopping Assistant"}'
```

The merchant then binds a customer's saved payment method by its own
reference for it; Enquoir keeps only that, the PSP's name and display
details, and returns its own binding ID. Card data is refused - an
undocumented field, or anything shaped like a card number, is a 400:

```sh
curl -H "Authorization: Bearer $MERCHANT_KEY" -X POST localhost:8090/v1/commerce/funding-methods \
  -d '{"customer_id":"cus_...","provider":"stripe","merchant_funding_method_id":"mfm_123",
       "display":{"brand":"visa","last_four":"4242"}}'      # {"id":"fmb_...", ...}
```

The merchant always comes from the key, never from the request; another
merchant's records (another storefront's too) are not found. An agent
key and a merchant key are never accepted in each other's place.
Disabling an integration, or rotating or revoking its key, ends access
at once. `make e2e-commerce` runs the whole flow (no sandbox needed).

## The end-to-end test

```sh
make e2e
```

Runs `scripts/e2e.sh` against a running worker and API, as a Cloud
consumer: signs up, creates an agent, checks the payment routes refuse a
missing or made-up key, then with the agent's key against the real
sandbox: edge validation, registers a payee (twice, same ID), a policy
block and its resubmission, pays the payee (submitted twice, the second
with a reworded reason: one key, one transfer), drives the transfer to
PAID through the simulator, and checks the intent settles to `APPLIED`
with a clean history. Then a second organisation proves it can neither
pay nor see the first's payee or payment, and gets its own beneficiary
for the same details. In between: the owner verifies their email from
the link Resend delivered, checks the dashboard (history, detail,
attempts, replay, analytics), invites a viewer who can look but not act,
reads the audit log, and the viewer resets their password from an
emailed link. 25 steps. Needs the worker to keep a copy of sent mail
(`MAIL_FILE`). Every step
is a plain, copyable curl. Needs `make up`, `make sandbox-setup`, `make
worker` and `make api`. Run only one worker: an old one still polling the
task queue will pick up work with stale code.

## The chaos test

```sh
make chaos
```

For each of four points in the money-moving call (`before_log`,
`after_log_before_call`, `during_call`, `after_call_before_commit`), this
submits one intent three times, runs a worker armed to exit hard at that
exact point (`ENQUOIR_CHAOS_KILL_AT`), checks it really died there (exit
1 plus the kill switch's log line), restarts a clean one, drives the
resulting transfer to PAID, and asserts the intent reached `APPLIED`, its
hash chain still verifies, and the rail holds exactly one transfer for
that request ID - the same one the workflow recorded. Prints a results
table and exits non-zero if anything failed. It talks to the real sandbox
and runs a freshly built worker binary as its subprocesses. `make up` and
`make sandbox-setup` must have already run, and **no other worker may be
running**: it would take the armed worker's work.

Passed all four points against the real sandbox on 2026-11-06; see
docs/progress.md for the results and what each point proves.
`during_call` kills the worker inside its HTTP transport the moment the
transfer create has fully left the process, before any response is read.

## Architecture

```
cmd/api        HTTP server (chi) - thin handlers: parse, call a use case, respond
cmd/worker     Temporal worker: the payment workflow + its activities
cmd/demo       one-shot: submit, pay through the simulator, print the result
cmd/chaos      the kill test
cmd/probe      throwaway sandbox exploration (see docs/rail-notes.md)
cmd/sandboxsetup   idempotent: funded account + one test beneficiary

internal/domain    Intent, Money, IntentKey, the state machine, hash-chained events -
                    imports nothing outside the standard library
internal/app       use cases (SubmitIntent, GetIntent) and the ports they depend on
                    (IntentRepository, EventLog, Rail, Simulator, Clock, PaymentStarter)
internal/commerce  the commerce domain (merchants, customers, agents; mandates
                    to come) - standard library and internal/domain only
internal/commerce/commerceapp  commerce use cases and ports, over the
                    shared infrastructure in internal/app; treasury never
                    imports commerce
internal/workflow  the Temporal workflow and its activities - deterministic
                    orchestration only; every I/O call is an activity
internal/chaos     the one deliberate exception to the architecture: a kill
                    switch with no port, because its only job is os.Exit
internal/adapters  Postgres (repositories + embedded migrations), Airwallex
                    (Rail + Simulator, two separate types so nothing holding
                    only a Rail can reach a sandbox-only call), httpapi, clock
```

The core guarantee: an event is durably recorded *before* the Rail is
ever called, and no outcome is retried until it's been looked up by
request ID first. The intent's key - a stable hash of its normalised
fields - is both the Temporal workflow ID (so a resubmission attaches to
the run already in progress) and the Airwallex `request_id` (so a
resubmission is recognised as the same payment, not a new one).

## What's verified, and what isn't

Verified directly, not just "should work":

- `make up` brings Postgres, Temporal and its UI up healthy from a clean
  volume.
- `make test` passes offline - checked with a stripped shell environment
  (no network, no env vars).
- `make test-integration` passes against the real Postgres and Temporal
  from `make up`, including: the architecture guard actually catches a
  real violation before going green; Postgres dedup and gapless event
  sequencing; starting the same intent twice attaching to the running
  workflow, including under a 20-way concurrent duplicate storm (fake
  Rail, real Temporal - the property under test is the server's
  workflow-ID dedup, not the sandbox).
- The payment workflow's seven core behaviours (happy path to `APPLIED`;
  over-maximum to `BLOCKED` without ever calling the Rail; `IN_FLIGHT`
  recorded before the Rail is called; an ambiguous create resolved by
  lookup with no second create sent; a confirmed-absent ambiguous create
  retried with the exact same request ID; a rejection ending `FAILED`;
  a cancelled transfer ending `FAILED` with the reason recorded) and an
  eighth added once the chaos kill points surfaced the gap: an
  *unclassified* activity failure - what a bare worker crash actually
  looks like - recovers the same way a classified ambiguous one does,
  instead of failing the run.
- The Airwallex adapter's behaviour against a local `httptest` server:
  token caching/refresh, unit conversion, the request ID passed through
  unchanged, status mapping, backoff on a 429, and `duplicate_request_id`
  resolving via lookup instead of surfacing as an error.
- The HTTP API against the real Postgres/Temporal stack (no sandbox
  needed - the API only starts workflows, it never calls the rail
  itself): a valid submission, the same intent twice returning the same
  key, five flavours of invalid input, fetching state and history,
  not-found, and `/healthz`.
- `internal/chaos`'s kill switch really does `os.Exit(1)` when armed
  (verified via a subprocess re-exec of the test binary, the one way to
  observe that from inside `go test`) and is a no-op otherwise; `go run`
  faithfully forwards a child's `os.Exit(1)` as its own exit code
  (checked directly with a throwaway program) - the assumption
  `cmd/chaos` depends on to know an armed worker died as expected.

Verified against the real Airwallex sandbox (2026-11-06):

- `make sandbox-setup`, `make demo`, `make e2e` (all 25 steps) and
  `make chaos` (all four kill points). Where the real sandbox differs
  from the documented facts is recorded in docs/rail-notes.md.
- A retry with a reworded `reason` resolves to the same intent and
  leaves exactly one transfer on the rail.
- In the `during_call` and `after_call_before_commit` chaos points, the
  worker's create really landed on Airwallex before it died (with no
  response read, and with a response read but not recorded), the
  retried create reused the request ID, and the rail still held one
  transfer.

Not yet verified:

- A `PAID` transfer failing afterwards (needs webhooks, out of scope).
