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

Posting the exact same intent again (same payee, amount, currency,
reference and reason) returns the same key and starts nothing new -
Temporal's own workflow-ID deduplication, since the intent's key is the
workflow's ID. `GET` 404s until a worker has actually processed at least
one activity for that intent; nothing recorded yet is nothing observed,
by design.

## The chaos test

```sh
make chaos
```

For each of four points in the money-moving call (`before_log`,
`after_log_before_call`, `during_call`, `after_call_before_commit`), this
submits one intent three times, runs a worker armed to exit hard at that
exact point (`ENQUOIR_CHAOS_KILL_AT`), restarts a clean one, drives the
resulting transfer to PAID, and asserts the intent never rested in
`UNKNOWN`, its hash chain still verifies, and the rail still holds the
one transfer for that request ID. Prints a results table and exits
non-zero if anything failed. It talks to the real sandbox and runs real
`go run ./cmd/worker` subprocesses - `make up` and `make sandbox-setup`
must have already run.

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

Not yet verified - built and believed correct, but never actually run:

- `cmd/probe`'s findings against the real sandbox (login shape,
  idempotent-retry behaviour, the exact duplicate-request-id and
  simulator-transition semantics) - `docs/rail-notes.md` doesn't exist
  yet. The Airwallex adapter's own behaviour is pinned by its
  `httptest` tests either way; what probing would add is confidence
  that those tests model the real sandbox's actual shape, not just
  AGENTS.md's documented facts about it.
- `make sandbox-setup`, `make demo`, and `make chaos` against the real
  sandbox - all three need `AWX_CLIENT_ID`/`AWX_API_KEY`, which were
  still unset in `.env` as of this phase. Everything upstream of that
  credential (the adapter, the workflow, the API, the kill points
  themselves, the orchestration logic in `cmd/chaos`) has been verified
  on its own terms, as listed above - what's left is the one end-to-end
  run against the actual sandbox.

If you're picking this up with real credentials in hand: `make
sandbox-setup`, then `make demo`, then `make chaos` is the order that
proves the whole chain, cheapest first.
