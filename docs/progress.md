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
