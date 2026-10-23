# Progress

## Phase 1: foundation, domain, event history — done

### What exists

- **Scaffold.** Go module `enquoir`, the directory layout from AGENTS.md,
  `.gitignore`, `.env.example`, a `Makefile` (`up`, `down`, `test`,
  `test-integration`, `lint`), Dockerfiles for `cmd/api` and `cmd/worker`
  (not yet wired into Compose - nothing to containerize until those
  commands have real logic), and `deploy/docker-compose.yml` running
  Postgres 16, `temporalio/auto-setup:1.29.1` and `temporalio/ui:2.34.0`.
  One Postgres container holds three databases: `enquoir` (the app) and
  `temporal` / `temporal_visibility` (Temporal's own persistence),
  created by an init script since the auto-setup user isn't `CREATEDB`.
  Dropped `admin-tools` from the upstream `temporalio/docker-compose`
  reference to keep the slice small.
- **Architecture guard** (`internal/archtest`): fails if `internal/domain`
  imports anything outside the standard library, or if `internal/app`
  imports an adapter. Parses source with `go/parser`, so it still works
  even if the guarded package doesn't compile.
- **`domain.Money`**: integer minor units + `Currency` (USD/EUR/GBP only).
  Rejects zero/negative amounts and unsupported currencies.
  `ParseMajorUnits`/`MajorUnitsString` convert to and from decimal
  strings with no float involved.
- **`domain.Intent` / `domain.IntentKey`**: `NewIntent` normalises its
  reference (trimmed) and currency (upper-cased), then derives a
  32-character lowercase-hex key from the SHA-256 of its normalised
  fields. A golden test pins a known key for a fixed input.
- **`domain.State`**: PROPOSED, AUTHORIZED, IN_FLIGHT, UNKNOWN, APPLIED,
  FAILED, BLOCKED, with a table-driven legal-transition graph. APPLIED
  and BLOCKED are terminal; FAILED is not - a reconciler can
  re-authorize a failed attempt or block it outright.
- **`domain.Event` / `domain.Chain`**: hash-chained events; `Chain.Verify`
  detects a tampered event or a missing one, naming the exact sequence
  number.
- **`internal/app.IntentRepository` / `EventLog`** ports, and their
  Postgres implementations (`internal/adapters/postgres`), with embedded
  migrations for `intents` (unique on `key`) and `intent_events`.

### What I verified

- `make up` brings Postgres, Temporal and its UI up healthy from a clean
  volume (`docker compose down -v && make up`).
- `make test` passes with no network and no env vars set (checked with a
  stripped shell environment).
- `make test-integration` passes against the Compose Postgres: saving
  the same intent key twice leaves exactly one row; `EventLog.Append`
  produces gapless sequence numbers; a chain reloaded from storage still
  verifies.
- The architecture guard actually catches a violation: added a temporary
  file importing `internal/app` from `internal/domain`, confirmed the
  guard failed with the right message, removed it, confirmed green.
- `gofmt -l .` and `go vet ./...` (and `go vet -tags=integration ./...`)
  are clean.

### What I did not verify

- The Dockerfiles for `cmd/api` and `cmd/worker` haven't been built - there's
  nothing in those commands yet worth containerizing.
- `EventLog.Append`'s load-then-insert isn't safe under concurrent
  appends to the *same* intent key (two concurrent callers could both
  compute the same next sequence number); the composite primary key
  would turn that into an insert error rather than silent corruption,
  but it hasn't been tested under concurrency. Worth revisiting once
  Phase 2's workflow activities can actually append concurrently.
- No `state` column exists on `intents` yet - nothing in Phase 1 needed
  to persist an Intent's current `State`, so I left it out rather than
  add an unused column.

### Deviations / notes

- The `Rail` and `Clock` ports AGENTS.md's architecture section lists
  alongside `IntentRepository`/`EventLog` aren't defined yet - nothing in
  Phase 1's scope needed them, and `Rail` is tied to the Airwallex
  integration this phase explicitly excludes.
- The first commit ended up bundling the scaffold with the architecture
  guard test (a staging mistake - files stayed staged across an
  unrelated `git add`), rather than landing as two separate commits.
  Not corrected by rewriting history, per the no-amend/no-rebase rule;
  every commit since has been one behaviour each.
- Two infrastructure problems surfaced only once things actually ran,
  and were fixed forward in their own commits: the Temporal healthcheck
  dialed `127.0.0.1`, which `auto-setup` never binds to (fixed by
  addressing it as `temporal:7233`); and both the UI's port 8080 and
  Postgres's port 5432 collided with other services already running on
  this machine (moved to 8088 and 5433).
- The Postgres adapter's code and its integration tests were written
  together instead of strictly test-first, since their correctness only
  means something against a live schema. I still verified behaviour
  directly by running the suite, including the duplicate-key and
  reload-then-verify paths, rather than assuming it worked.

### Open questions for Phase 2

- Where does an Intent's current `State` live, and who's allowed to move
  it - a dedicated use case in `internal/app`, presumably, but it wasn't
  built yet since nothing called for it.
- The `Rail` and `Clock` ports' exact shapes will depend on what the
  policy engine and workflow need from them; deferring their design
  until that phase, as agreed.

## Phase 2: Airwallex adapter and the durable workflow - code complete, sandbox verification pending

Interrupted mid-phase: probing the real sandbox, `make sandbox-setup`,
and `make demo` were all paused (at the user's request) because
`AWX_CLIENT_ID`/`AWX_API_KEY` were empty in `.env`, with instructions to
move on to Phase 3 regardless. Everything that doesn't need live
credentials is built and verified; the phase was never formally closed
(no `docs/rail-notes.md`, no `phase-2` tag) and Phase 3 picks up from
here.

### What exists

- **`internal/app.Rail`, `Simulator`, `Clock`** ports. `Rail` is the
  only thing that moves money, keyed by the intent's key as the request
  ID; `Simulator` is a separate interface so sandbox-only state changes
  can never leak in through something that only depends on `Rail`.
- **`internal/adapters/airwallex`**: `Client` (implements `Rail`) and
  `Simulator` (implements `Simulator`), two separate types with their
  own transport and token cache each. Token caching/refresh,
  minor-to-major conversion, request-ID pass-through, status mapping
  (`SENT` not final, `CANCELLED` carrying `failure_type`/
  `failure_reason`), 429 backoff, and `duplicate_request_id` resolving
  via lookup instead of surfacing as an error - all test-first against
  a local `httptest` server, shaped by AGENTS.md's documented facts
  (not yet the real sandbox's).
- **`cmd/probe`**: throwaway tooling to call the real sandbox and print
  what comes back. Written, never run.
- **`make sandbox-setup`** (`cmd/sandboxsetup`): idempotent funded USD
  Global Account + one US/USD/LOCAL test beneficiary, via
  `Simulator.EnsureBeneficiary` (not part of the `Simulator` port - a
  concrete-only method, since beneficiary bootstrapping is one-time
  infrastructure, not something the running system ever does itself).
- **`internal/workflow`**: `PaymentWorkflow` and `Activities`. The
  intent's key is the workflow ID. Every state move goes through
  `domain.State.Transition` before its event is recorded. A worker
  crash with nothing to classify gets the same recovery as a
  classified ambiguous outcome (see the fix below).
- **`cmd/worker`**, **`cmd/demo`**: the real worker, and a self-contained
  demo that runs its own in-process worker, submits one intent, drives
  it to `PAID` through the simulator, and prints the result.

### What I verified

- All seven of the workflow's core behaviours, test-first with
  Temporal's test suite, a fake `Rail`, and in-memory repositories.
- An eighth, added after a design review while building Phase 3's
  chaos kill points (see the fix below): an unclassified `CreateTransfer`
  failure recovers exactly like a classified ambiguous one.
- Starting the same intent twice attaches to the running workflow -
  against the real Temporal dev server, since the lightweight test
  suite doesn't model the server's own workflow-ID deduplication.
- The Airwallex adapter's behaviour, as listed above, against `httptest`.
- `make test` stays offline-clean throughout.

### A bug found and fixed along the way

`domain.Money`'s fields are unexported by design - every instance must
pass through `NewMoney`'s checks - but that also hid them from the
default JSON marshaller. Temporal's data converter serialises every
workflow/activity payload as JSON, so `Intent.Amount` was silently
zeroing out crossing that boundary: an over-the-maximum intent was
still reaching the rail, because the policy check saw `0`, not the real
amount. Fixed with a custom `MarshalJSON`/`UnmarshalJSON` on `Money`
that still goes through `NewMoney`, so a zeroed or invalid value can't
round-trip silently either.

### What I did not verify

- Everything that needs `AWX_CLIENT_ID`/`AWX_API_KEY`: the probe's
  findings against the real sandbox, `make sandbox-setup`, and
  `make demo` end to end. `docs/rail-notes.md` does not exist yet.
- Whether the real sandbox's behaviour actually matches the
  `httptest`-based assumptions the adapter's tests encode (field
  names, the exact shape of a `duplicate_request_id` response, what
  status a freshly created transfer actually starts in).

## Phase 3: HTTP API and the no-double-pay proof - code complete, chaos run against the real sandbox pending

Same blocker as Phase 2: `AWX_CLIENT_ID`/`AWX_API_KEY` are still empty
in `.env` as of this phase, so `make chaos` (this phase's actual
done-criterion) has not been run. Everything else is built and
verified on its own terms.

### What exists

- **`internal/app.SubmitIntent`/`GetIntent`** use cases, and
  **`PaymentStarter`** port (starts or attaches to the durable workflow).
  `SubmitIntent` reports the intent's currently-recorded state (read
  from the event log, not guessed), so attaching to a run already far
  along reports its real progress, not a stale `PROPOSED`.
- **`internal/adapters/httpapi`**: `POST /v1/intents`, `GET
  /v1/intents/{key}`, `GET /healthz`, on chi. Handlers stay thin: parse,
  call the use case, respond. `TemporalPaymentStarter` implements
  `PaymentStarter` against a real `client.Client`.
- **`cmd/api`**, wired to the real Postgres and Temporal.
- **`internal/chaos`**: `ENQUOIR_CHAOS_KILL_AT`, four points
  (`before_log`, `after_log_before_call`, `during_call`,
  `after_call_before_commit`) around the `IN_FLIGHT`-then-`CreateTransfer`
  sequence. Deliberately outside the clean architecture - a kill switch
  with no port, since its only job is `os.Exit`.
- **`cmd/chaos`**: the kill test orchestrator - submits one intent three
  times, runs a real `go run ./cmd/worker` subprocess armed to die at
  one point, restarts a clean one, drives the transfer to `PAID`, and
  checks recovery.
- **A duplicate-storm test** (`internal/workflow`, `test-integration`):
  20 concurrent submissions of the same intent key against the real
  Temporal server (fake `Rail` - the property under test is the
  server's dedup under real concurrency, not the sandbox).
- **README.md**: setup, usage, architecture, and an honest verified/
  not-verified list.

### What I verified

- Designing the chaos kill points surfaced a real gap, fixed before
  building `cmd/chaos`: with every activity capped at one attempt, a
  bare worker crash during `CreateTransfer` (no classified error, just
  a timeout) made the workflow fail outright instead of ever reaching
  the ambiguous-outcome recovery already built for a classified
  `ErrAmbiguous`. Raised the retry budget to 3 (safe, since
  `Rail.CreateTransfer` is idempotent on the intent's key) and changed
  `handleCreateError` so only an explicitly classified rejection skips
  to `FAILED` - everything else, classified ambiguous or not, goes
  through the same `UNKNOWN`-then-lookup path. Proved it with a test
  where the fake `Rail` returns a plain unclassified error: Temporal
  retries it 3 times, then the workflow's own recovery takes over, and
  the intent still resolves cleanly to `APPLIED`.
- The HTTP API, test-first with `httptest` against the real router and
  real use cases (fakes for `EventLog`/`PaymentStarter`): a valid
  submission, the same intent twice returning the same key, five
  flavours of invalid input, state-and-history retrieval, not-found,
  and `/healthz`.
- `cmd/api` live against the real Postgres/Temporal stack (no sandbox
  needed): POST, GET, re-POST, cleaned up with a terminate.
- `internal/chaos`'s kill switch: a no-op when unset or pointed at a
  different point; a real `os.Exit(1)` when it matches, confirmed via a
  subprocess re-exec of the test binary - the only way to observe that
  from inside `go test`.
- The load-bearing assumption behind `cmd/chaos`'s whole design: that
  `go run` forwards a child process's `os.Exit(1)` as its own exit
  code. Confirmed directly with a throwaway program before trusting it.
- The duplicate-storm test, for real, against the live Temporal server:
  20 concurrent calls, one run ID, one `CreateTransfer` call, resolves
  to `APPLIED`.
- `make up` from a clean volume (`docker compose down -v && make up`)
  and `make test` offline, both re-confirmed at the end of this phase.

### What I did not verify

- `make chaos` against the real sandbox - this phase's stated
  done-criterion. Blocked on `AWX_CLIENT_ID`/`AWX_API_KEY`; no results
  table exists yet because the run that produces one hasn't happened.
- By extension, whether the four kill points' real-world timing
  (especially `during_call`'s 50ms race against the actual HTTP call)
  behaves as intended against real network latency rather than the
  reasoning that designed it.
- Phase 2's unresolved items above still stand: the probe, rail notes,
  `sandbox-setup`, and `demo`, all against the real sandbox.

### Next step

Once real sandbox credentials are available: `make sandbox-setup`,
`make demo`, then `make chaos`, cheapest first - each one exercises
more of the stack than the last. `make chaos`'s results table belongs
in this file once it exists, and this phase's tag should wait for it:
the stated done-criterion is `make chaos` actually passing, not just
built.

## Beneficiary registration (added outside the phase plan, at the user's request)

Airwallex only pays a beneficiary it already holds, so an intent's
`payee` had to be a pre-registered ID with no way to get one except
`make sandbox-setup`. `POST /v1/beneficiaries` now registers one and
returns `{id, key}`.

### Design choices

- **Contract from the docs, not guessed.** Queried the Airwallex docs
  MCP server for `POST /beneficiaries/create` and `GET /beneficiaries`.
  US/USD/LOCAL uses `local_clearing_system: "ACH"`, `aba` routing, and
  a full address (every US example sends one; validation errors cite
  address fields).
- **No `request_id` on beneficiary create.** So Enquoir derives a
  `BeneficiaryKey` from the trimmed details, sends it as `nickname`,
  and looks it up via `GET /beneficiaries?nick_name=` (exact match
  only) before ever creating. This matters for money: an intent's key
  includes its payee ID, so one real payee under two IDs would let one
  invoice be paid twice.
- **Durable workflow, keyed `beneficiary-<key>`.** Concurrent
  registrations of the same payee attach to one run; the prefix keeps
  it apart from payment workflow IDs.
- **`CreateBeneficiary` gets one attempt.** Unlike `CreateTransfer`,
  a Temporal-level retry isn't idempotent here. Any failure short of a
  clear rejection loops back to the lookup. A mutation check (retries
  re-enabled) made the never-retried test fail with 3 creates.
- **Scope:** US company accounts, USD, ACH only. The key covers every
  field, so corrected details register a new beneficiary rather than
  silently reusing one with stale details.

### Verified

- Unit tests for every layer, offline (`make test`).
- Live against the Compose stack: the route starts the workflow, the
  worker runs it, and with the sandbox refusing login (403) the lookup
  fails and no create is attempted.

### Not verified

- Against the real sandbox: the login still returns 403 with the
  current `.env` keys. Whether the real create accepts this payload,
  and whether `nick_name` filtering behaves as documented, is unproven.
- `make sandbox-setup`, `demo` and `chaos` after the switch to
  `sandbox.EnsurePayee` (see below) - same 403 blocker.

## Fixes from the end-to-end run (same branch)

Running the system end to end against the Compose stack (sandbox login
refused with 403 throughout) surfaced these; each is its own commit.

- **Resubmitting a finished intent started a second run.** Reproduced
  live: an over-limit intent's log read PROPOSED, BLOCKED, PROPOSED,
  BLOCKED. Temporal only dedupes a workflow ID while it runs; for an
  APPLIED intent past Airwallex's 7-day `request_id` window, that is a
  double payment. `SubmitIntent` now starts a run only when the event
  log holds nothing for the key, and starts nothing if the log can't
  be read. Re-verified live: one run, clean log.
- **An unclear lookup stranded the intent in UNKNOWN.** The lookup
  after an unclear create, and the status poll while IN_FLIGHT, failed
  the run on their first error. Both now retry with backoff capped at
  5 minutes. Re-verified live with login refused: the run stays
  Running in UNKNOWN, no second create. The retry is unbounded by
  design; a long outage grows the workflow's history slowly, and
  continue-as-new hasn't been added.
- **Transfer lookup took `items[0]`.** It now matches `request_id`
  exactly and returns `ErrDuplicateTransfers` on more than one match,
  which stops the run (non-retryable) for a person to look at. This
  also makes `cmd/chaos` assert exactly one transfer, not merely one.
  Its test fake lacked the documented `request_id` field; corrected in
  its own commit first.
- **The sandbox payee payload contradicted the docs** ("LOCAL", no
  address, matched by company name). Replaced `Simulator.EnsureBeneficiary`
  with `internal/sandbox.EnsurePayee`, which uses the same
  `BeneficiaryRegistry` calls as the API.
- **`cmd/probe` printed the bearer token.** Now redacted; its payee
  payload also follows the docs.

Left as is: `BeneficiaryWorkflow` still fails fast when its lookup
errors. It backs a synchronous API call, so the caller gets a 500 it
can safely retry, rather than a request that waits out an outage.
