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

Register the payee first. Airwallex only pays a beneficiary it already
holds, so an intent's `payee` is the `id` this returns:

```sh
curl -X POST localhost:8090/v1/beneficiaries \
  -H 'Content-Type: application/json' \
  -d '{"name":"Acme Ltd","account_number":"50001121","routing_number":"021000021",
       "address":{"street":"412 5th Avenue","city":"Seattle","state":"WA","postcode":"98104"}}'
# {"id":"<beneficiary id>","key":"..."}
```

Only US company accounts paid in USD over ACH are supported for now.
Registering the same details again returns the same `id`: Airwallex's
beneficiary create has no idempotency key, so Enquoir stores its own key
as the beneficiary's nickname and looks it up before ever creating one.
A rail rejection comes back as a 422 carrying Airwallex's reason.

```sh
curl -X POST localhost:8090/v1/intents \
  -H 'Content-Type: application/json' \
  -d '{"payee":"<beneficiary id>","amount":"10.50","currency":"USD","reference":"INV-1001","reason":"invoice settlement"}'
# {"key":"...","state":"PROPOSED"}

curl localhost:8090/v1/intents/<key>
# {"key":"...","state":"APPLIED","events":[...]}

curl localhost:8090/healthz
# ok
```

Posting the same intent again (same payee, amount, currency and
reference) returns the same key, reports its recorded state, and starts
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

## The end-to-end test

```sh
make e2e
```

Runs `scripts/e2e.sh` against a running worker and API: edge validation,
a policy block, resubmission of a finished intent, then against the real
sandbox it registers a payee (twice, same ID), pays it (submitted twice,
one transfer), drives the transfer to PAID through the simulator, and
checks the intent settles to `APPLIED` with a clean history. Every step
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
docs/progress.md for the results and what each point does and doesn't
prove (`during_call`'s kill currently fires before the request leaves).

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

- `make sandbox-setup`, `make demo`, `make e2e` (all 13 steps) and
  `make chaos` (all four kill points). Where the real sandbox differs
  from the documented facts is recorded in docs/rail-notes.md.
- A retry with a reworded `reason` resolves to the same intent and
  leaves exactly one transfer on the rail.
- In the `after_call_before_commit` chaos point, the worker's create
  really landed on Airwallex before it died, the retried create reused
  the request ID, and the rail still held one transfer.

Not yet verified:

- A worker killed while its create request is genuinely in the air:
  `during_call`'s kill fires before the request leaves (details in
  docs/progress.md).
- A `PAID` transfer failing afterwards (needs webhooks, out of scope).
