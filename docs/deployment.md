# Enquoir deployment handoff

Everything needed to deploy Enquoir on any cloud platform. It is written
for the engineer doing the deployment and assumes no knowledge of the
codebase.

> **Status, Nov 2026.** Everything below was checked against `main`: both
> images were built and run against Postgres, Temporal and Redis, the API
> served sign-ups, and both processes shut down cleanly on `SIGTERM`. The
> one open decision is the reverse-proxy client-IP header (section 9),
> which depends on the platform you choose.

---

## 1. What you are deploying

Enquoir sits between AI agents and a payment provider (Airwallex, sandbox
only). Agents call Enquoir's HTTP API to make payments; people use the
same API (later through a web dashboard) to manage their organisation
and watch every payment.

```
                 Internet
                    │ HTTPS
          ┌─────────▼──────────┐
          │ Load balancer /    │  TLS ends here
          │ reverse proxy      │
          └─────────┬──────────┘
                    │ HTTP :8090
          ┌─────────▼──────────┐          ┌──────────────────────┐
          │  enquoir-api       │──────────▶  Redis 7              │ cache + rate limits
          │  (stateless)       │          └──────────────────────┘
          └───┬───────────┬────┘
              │           │ starts workflows (gRPC :7233)
              │     ┌─────▼──────────────┐
              │     │  Temporal server   │  PRIVATE: no authentication
              │     └─────▲──────────────┘
              │           │ polls task queue "enquoir-payments"
              │     ┌─────┴──────────────┐      outbound HTTPS
              │     │  enquoir-worker    │──────▶ Airwallex sandbox API
              │     │  (no inbound port) │──────▶ Resend email API
              │     └─────┬──────────────┘
              │           │
          ┌───▼───────────▼────┐
          │  PostgreSQL 16     │  databases: enquoir, temporal, temporal_visibility
          └────────────────────┘
```

| Component | What it is | You run it as |
|---|---|---|
| `enquoir-api` | HTTP API (Go). Stateless. | Container from `deploy/Dockerfile.api`, 1+ replicas, public via the load balancer |
| `enquoir-worker` | Runs the durable payment and email workflows (Go). Calls Airwallex and Resend. | Container from `deploy/Dockerfile.worker`, no inbound traffic |
| PostgreSQL 16 | The system of record: payments, their hash-chained history, accounts, audit log. Also Temporal's storage. | Managed Postgres recommended |
| Temporal server 1.29 | Durable workflow engine | Self-hosted, on a private network (section 6) |
| Redis 7 | Rate-limit counters and a 60-second session/API-key cache. **Not a record**: losing it loses nothing. | Managed or self-hosted; no persistence needed |
| Airwallex sandbox | Payment provider (external) | Nothing to run; needs an IP allowlist entry (section 7) |
| Resend | Email delivery (external) | Nothing to run; domain already verified (section 8) |
| Web dashboard | Not built yet. Will be served at `https://enquoir.etimi.me`. | Later |

---

## 2. Requirements at a glance

- A container runtime for two services (Kubernetes, ECS, Cloud Run,
  Fly.io, Render, a VM with Docker: anything works).
- **PostgreSQL 16**, reachable from the API, the worker and Temporal.
- **Temporal server 1.29.x**, reachable from the API and the worker
  **only**.
- **Redis 7**, reachable from the API only (the worker doesn't use it).
- **A fixed outbound (egress) IP for the worker**. Airwallex only accepts
  calls from allowlisted IPs (section 7). This is the requirement most
  likely to shape your choice of platform.
- DNS for `api.enquoir.etimi.me` (recommended, see section 9) and a TLS
  certificate for it.

Suggested starting sizes (low traffic, a hackathon/demo workload): API
and worker 0.25–0.5 vCPU and 256 MB each; Postgres the smallest managed
tier with backups; Redis the smallest tier; Temporal 1 vCPU, 1–2 GB.

---

## 3. Building the images

From the repository root, on `main`:

```sh
docker build -f deploy/Dockerfile.api    -t <registry>/enquoir-api:<git-sha>    .
docker build -f deploy/Dockerfile.worker -t <registry>/enquoir-worker:<git-sha> .
```

- Go 1.26, statically linked (`CGO_ENABLED=0`), on `alpine:3.20`. About
  24 MB (API) and 20 MB (worker).
- Both run as a non-root user (uid 10001), write nothing to disk and bind
  no privileged port.
- `.dockerignore` keeps `.env` and local files out of the build. **Never
  bake configuration or secrets into an image**: everything comes from
  environment variables.
- Tag images with the git commit SHA, not `latest`, so a rollback is a
  redeploy of the previous SHA.

---

## 4. Configuration

All configuration is environment variables. The `.env.example` file in
the repository documents the development values.

### 4.1 `enquoir-api`

| Variable | Required | Production value | Notes |
|---|---|---|---|
| `DATABASE_URL` | yes | `postgres://USER:PASS@HOST:5432/enquoir?sslmode=require` | **Secret.** Use `sslmode=require` (or `verify-full`) with managed Postgres. |
| `TEMPORAL_ADDRESS` | yes | `temporal.internal:7233` | Private address. Default `127.0.0.1:7233`. |
| `REDIS_URL` | strongly recommended | `redis://:PASS@HOST:6379/0` or `rediss://…` for TLS | **Secret if it holds a password.** Without it the API still works, but rate limits are per replica and nothing is cached (a warning is logged). |
| `HTTP_ADDR` | no | `:8090` | Listen address. Default `:8090`. |
| `SESSION_COOKIE_SECURE` | no | **leave unset** (means `true`) | Only `false` disables the Secure flag, for plain-HTTP local development. Never set `false` in production. |
| `SESSION_TTL` | no | `168h` | Dashboard session lifetime (Go duration). Default 7 days. |
| `CORS_ALLOWED_ORIGINS` | yes, once the dashboard exists | `https://enquoir.etimi.me` | Comma-separated browser origins allowed to call the API with cookies. Empty means no other origin. |
| `CLIENT_IP_HEADER` | depends on platform | e.g. `CF-Connecting-IP` | **Open decision, see section 9.** Empty means "use the TCP peer address". |
| `MAX_PAYMENT_MINOR_UNITS` | no | `100000000` | Policy ceiling per payment in cents (default = $1,000,000). Larger payments are blocked. |

The API never calls Airwallex or Resend, so it needs neither set of
credentials.

### 4.2 `enquoir-worker`

| Variable | Required | Production value | Notes |
|---|---|---|---|
| `DATABASE_URL` | yes | same as the API | **Secret.** |
| `TEMPORAL_ADDRESS` | yes | same as the API | |
| `AWX_BASE_URL` | yes | `https://api.sandbox.airwallex.com` | **Must be exactly this.** The worker refuses to start with any other value: real money is deliberately impossible. |
| `AWX_CLIENT_ID` | yes | from the team | **Secret.** |
| `AWX_API_KEY` | yes | from the team | **Secret.** |
| `EMAIL_PROVIDER` | yes | `resend` | The worker refuses to start without it. `file` is for development only. |
| `RESEND_API_KEY` | yes | from the team | **Secret.** |
| `EMAIL_FROM` | yes | `Enquoir <no-reply@enquoir.etimi.me>` | The domain is verified in Resend. |
| `APP_BASE_URL` | yes | `https://enquoir.etimi.me` | Where links in emails point: the dashboard (verify email, accept invitation, reset password). Defaults to this value if unset. |
| `MAIL_FILE` | **must be unset** | (unset) | With `resend`, setting it also writes a copy of every email, live links included, to a local file. Development only. |

### 4.3 Never set in production

| Variable | Why |
|---|---|
| `ENQUOIR_CHAOS_KILL_AT` | A test switch: it makes the worker deliberately kill itself mid-payment. The worker logs `CHAOS MODE ARMED` if it is set. |
| `MAIL_FILE` | Writes live login links to disk (see above). |
| `EMAIL_PROVIDER=file` | No email is sent; links go to a file. |
| `SESSION_COOKIE_SECURE=false` | Session cookies would travel over plain HTTP. |

Also: don't place a `.env` file in the API container's working
directory. The API loads one if it finds it (a development convenience).

### 4.4 Secrets

| Secret | Used by | Owner / where to get it |
|---|---|---|
| Postgres password (in `DATABASE_URL`) | API, worker, Temporal | you create it |
| Redis password (in `REDIS_URL`) | API | you create it |
| `AWX_CLIENT_ID`, `AWX_API_KEY` | worker, one-off sandbox setup | Airwallex sandbox account (ask the project owner) |
| `RESEND_API_KEY` | worker | Resend account (ask the project owner) |

Store them in your platform's secret manager and inject them as
environment variables. None of them is ever logged by the application.

---

## 5. PostgreSQL

**Databases.** Create three on the same server (or split them as you
prefer):

```sql
CREATE DATABASE enquoir;              -- the application
CREATE DATABASE temporal;             -- Temporal (if Temporal uses Postgres)
CREATE DATABASE temporal_visibility;  -- Temporal visibility
```

**User.** The application's user needs to create and alter tables in
`enquoir` (it runs its own migrations). A dedicated owner role for that
database is simplest.

**Migrations run automatically.** The API and the worker each apply any
pending migration at start-up. They take a Postgres advisory lock while
doing so, so any number of processes may start at once: the first
applies the migrations, the others wait and find them done. Migrations
are forward-only, and there is no "down".

**Backups are essential.** This database is the system of record: every
payment's tamper-evident history, the audit log, accounts. Enable
automated daily backups plus point-in-time recovery, and take a manual
snapshot before every release that adds a migration (new files in
`internal/adapters/postgres/migrations/`). Back up the Temporal databases
too: they hold in-flight workflows.

---

## 6. Temporal

Enquoir's payments and emails run as Temporal workflows, so Temporal must
be up for payments to progress. (If it is down, the API still answers,
but nothing new starts.)

- **Version:** server 1.29.x (local development uses
  `temporalio/auto-setup:1.29.1`). Go SDK 1.49.
- **Namespace:** `default`. Task queue: `enquoir-payments` (created
  automatically). Workflows: `PaymentWorkflow`, `BeneficiaryWorkflow`,
  `SendEmailWorkflow`.
- **Production install:** don't use the `auto-setup` image in production;
  Temporal recommends it only for development. Use Temporal's official
  Helm chart (Kubernetes) or the `temporalio/server` image with its
  schema set up by `temporal-sql-tool`, backed by the Postgres above.
  Make sure the `default` namespace exists.
- **Keep it private.** Enquoir connects to Temporal with a plain
  host:port: no TLS, no API key, no mTLS. Temporal's frontend (port 7233)
  has no authentication of its own, so anyone who can reach it could
  start a payment directly and bypass Enquoir's checks. Expose it only to
  the API and worker, on a private network, never publicly.
- **Temporal Cloud is not supported yet.** It needs TLS/API-key client
  options the code doesn't set. That's a small code change if you want
  it; ask the development team.
- **Temporal Web UI** (optional, `temporalio/ui`): useful for watching
  workflows. Keep it behind a VPN or your platform's private access; it
  has no login.

---

## 7. Airwallex (payments): sandbox only

- The worker talks to `https://api.sandbox.airwallex.com` and refuses any
  other URL. No real money can move.
- **IP allowlist (critical).** The Airwallex API key only works from
  allowlisted IP addresses. A call from any other IP gets
  `403 {"code":"unauthorized"}`, which looks exactly like a wrong key.
  This has already bitten the team once. So:
  1. Give the worker a **fixed egress IP** (a NAT gateway with a static
     IP, a static outbound IP add-on, or a VM with a fixed address).
  2. Send that IP to the project owner to add to the Airwallex key's
     allowlist **before** go-live.
  3. Any machine that runs the one-off setup below needs an allowlisted
     IP too.
- **Fund the sandbox account (one-off job).** Transfers draw on the
  sandbox balance. Run the setup command once at launch, and again
  whenever transfers start failing for lack of funds. It is safe to
  repeat; each run deposits a further $1,000 of sandbox money and
  creates a demo organisation and test payee if they're missing.

  ```sh
  # from a checkout of the repo, with Go 1.26, the production DATABASE_URL,
  # the Airwallex variables, and an allowlisted IP:
  go run ./cmd/sandboxsetup
  ```
  If you'd rather not run Go on a laptop, run the same command as a
  one-off job in a `golang:1.26-alpine` container with the repository
  mounted, from inside your network.
- Airwallex limits: 10 concurrent requests per account. Each worker
  process caps itself at 10 (see section 11 on replicas).

---

## 8. Email (Resend)

- Sender: `Enquoir <no-reply@enquoir.etimi.me>`. The domain
  `enquoir.etimi.me` is **verified in Resend**. Its DNS records (the SPF,
  DKIM and any other records Resend asked for) must stay in place. If
  you move DNS, copy them across, or email stops.
- Emails are durable: each is recorded in Postgres, then sent by a
  workflow that retries for up to 23 hours. The worker also re-sends,
  every 5 minutes, any email left queued (for example because Temporal
  was briefly down). Nothing secret is stored: the database keeps only a
  template ID, and links are minted at send time.
- Emails link to the dashboard at `APP_BASE_URL`:
  `/verify-email`, `/invitations/accept`, `/reset-password`. Until the
  dashboard is deployed, those links will 404.

---

## 9. Domains, TLS, cookies and the reverse proxy

### Hostnames (recommended)

| Host | Serves |
|---|---|
| `enquoir.etimi.me` | The dashboard (frontend, later). Email links point here. |
| `api.enquoir.etimi.me` | `enquoir-api` |

**Keep the API on the same site as the dashboard** (a subdomain of
`etimi.me`, as above). The session cookie is `SameSite=Lax`, which
browsers only send on same-site requests. If the API lives on an
unrelated domain (e.g. `something.run.app`), the dashboard's requests
won't carry the session and sign-in will appear to fail. Then set
`CORS_ALLOWED_ORIGINS=https://enquoir.etimi.me`.

### TLS

Terminate TLS at the load balancer and forward plain HTTP to the API on
port 8090. Redirect HTTP to HTTPS. Leave `SESSION_COOKIE_SECURE` unset.

### Load balancer settings

| Setting | Value | Why |
|---|---|---|
| Health check | `GET /healthz` → `200 ok` | Plain liveness check |
| Request/idle timeout | **≥ 90 seconds** | Registering a payee can wait up to 60 s on its workflow; the API's own write timeout is 75 s |
| Max request body | ≥ 64 KiB | The API rejects bodies over 64 KiB itself (`413`) |
| Connection draining / termination grace | ≥ 35 seconds | On `SIGTERM` the API finishes in-flight requests for up to 30 s |

### Client IP header (open decision: settle it here)

The API rate-limits sign-up (5 an hour) and log-in (30 a minute) **per
client IP**. Behind a proxy, every request arrives from the proxy's own
address, so you must tell the API which header carries the real client
IP, via `CLIENT_IP_HEADER`.

**How the API reads it:** it takes the **first** comma-separated value of
that header. So the header must be one that **your edge proxy sets
itself, overwriting anything the client sent**. If the proxy appends to a
client-supplied header instead (common with `X-Forwarded-For`), a
client could put any IP first and dodge the limits.

What to do:
1. Pick a header your platform's edge sets and overwrites. Examples:
   Cloudflare's `CF-Connecting-IP`, Fly.io's `Fly-Client-IP`, or an
   nginx you control configured with
   `proxy_set_header X-Real-IP $remote_addr;`.
2. Set `CLIENT_IP_HEADER` to that header name.
3. **Test it:** send a request with a forged value of that header and
   confirm the API doesn't use it:
   ```sh
   for i in 1 2 3 4 5 6; do
     curl -s -o /dev/null -w '%{http_code} ' -X POST https://api.enquoir.etimi.me/v1/auth/signup \
       -H 'Content-Type: application/json' -H "<HEADER>: 198.51.100.$i" \
       -d "{\"email\":\"iptest$i-$(date +%s)@example.com\",\"password\":\"correct horse battery\",\"organisation_name\":\"t\"}"
   done
   # Expect: 201 201 201 201 201 429. Six 201s mean the forged header was trusted.
   ```
   (This creates throwaway accounts; tell the project owner so they can be
   ignored.)
4. If the API is reached directly with no proxy, leave
   `CLIENT_IP_HEADER` empty.

---

## 10. First deployment, step by step

1. **Provision** Postgres (with the three databases), Redis and a
   private network. Enable Postgres backups.
2. **Install Temporal** privately against Postgres (section 6) and
   confirm the `default` namespace exists.
3. **Arrange the worker's static egress IP** and have it added to the
   Airwallex allowlist (section 7).
4. **Store the secrets** (section 4.4) in the secret manager.
5. **Deploy `enquoir-api`** (1 replica to start) behind the load
   balancer, with the settings in section 9. It applies the database
   migrations on first start.
6. **Deploy `enquoir-worker`** (1 replica).
7. **Run the sandbox setup job** once (section 7).
8. **Point DNS** for `api.enquoir.etimi.me` at the load balancer.
9. **Settle `CLIENT_IP_HEADER`** and run its test (section 9).
10. **Smoke test** (section 13).

Start order doesn't matter for correctness: either service can start
first, and migrations are locked.

---

## 11. Scaling

- **API:** stateless; scale horizontally as needed. Redis keeps rate
  limits shared across replicas. Without Redis, each replica counts on
  its own.
- **Worker:** Temporal shares work between replicas safely, and no
  payment is processed twice. But each worker process allows up to 10
  concurrent Airwallex calls, and the sandbox account allows 10 in
  total, so **run one worker replica** for now (two for availability is
  acceptable at low volume; you may then see occasional rate-limit
  retries, which are handled). Every worker replica's egress IP must be
  allowlisted.
- **Postgres** is the bottleneck long before the API is; watch its
  connections (each process holds a small pool).

---

## 12. Monitoring and logs

**Logs** are structured JSON on stdout (one object per line, fields
`time`, `level`, `msg`, …). Ship them to your platform's log store. They
never contain passwords, tokens or API keys.

**Alert on these log messages:**

| `msg` contains | Meaning | Action |
|---|---|---|
| `redis rate limiting failed` | Redis unreachable; limits fall back to per-process | Fix Redis; nothing is lost |
| `REDIS_URL not set` or `redis unreachable at start-up` | API started without Redis | Check `REDIS_URL` |
| `configuring email` (level ERROR) | Worker can't start: email misconfigured | Check the `EMAIL_*` variables |
| `missing required environment variable` | Process can't start | Check that variable |
| `CHAOS MODE ARMED` | Test switch set in production | Remove `ENQUOIR_CHAOS_KILL_AT` immediately |
| `EMAIL_PROVIDER=file` | Emails not being sent | Set `EMAIL_PROVIDER=resend` |
| `session cookies are not Secure` | `SESSION_COOKIE_SECURE=false` in production | Unset it |
| `airwallex: login: status 403` (in worker/Temporal activity errors) | Worker IP not allowlisted, or key wrong | Check the egress IP is allowlisted |

**Health:**
- API: `GET /healthz`.
- Worker: no HTTP endpoint. Use process liveness, and check in the
  Temporal UI that the `enquoir-payments` task queue has pollers.

**Useful queries** (read-only, on the `enquoir` database):

```sql
-- Payments stuck with an unclear outcome for over 15 minutes (should be rare; Enquoir keeps retrying)
SELECT key, org_id, state_changed_at FROM intents
WHERE state = 'UNKNOWN' AND state_changed_at < now() - interval '15 minutes';

-- Emails that failed for good, in the last day
SELECT id, template, error, created_at FROM emails
WHERE status = 'failed' AND created_at > now() - interval '1 day';

-- Airwallex calls cut off mid-flight (a worker died) in the last hour
SELECT count(*) FROM attempts
WHERE kind = 'rail_call' AND outcome = 'in_progress' AND started_at < now() - interval '2 minutes'
  AND started_at > now() - interval '1 hour';
```

---

## 13. Smoke test after a deployment

```sh
API=https://api.enquoir.etimi.me

curl -s $API/healthz                                  # ok
curl -s -o /dev/null -w '%{http_code}\n' $API/docs   # 200 (API reference page)

# Sign up with an inbox you can read: a verification email should arrive
# from no-reply@enquoir.etimi.me within a minute.
curl -s -c /tmp/c -X POST $API/v1/auth/signup -H 'Content-Type: application/json' \
  -d '{"email":"you+smoke@yourdomain","password":"correct horse battery","organisation_name":"Smoke test"}'
curl -s -b /tmp/c $API/v1/auth/session                # 200, your user, role "owner"
```

The email's link points at the dashboard, which doesn't exist yet. To
complete verification by hand, post its `token` to
`POST /v1/auth/verify-email` with body `{"token":"…"}`.

The full payment path (agent → Airwallex → paid) is covered by the
development team's `make e2e` against a local stack. For a production
check, ask them for a guided run: it needs an agent key and sandbox
simulator calls.

---

## 14. Releases, upgrades and rollbacks

- **Release:** build new images from a `main` commit, then roll out the
  API and the worker. If the release adds migrations, take a database
  snapshot first; the new version applies them at start-up.
- **Rollback:** redeploy the previous image SHA. That is safe when the
  release added no migration. Migrations are not reversible, and an
  older version may not run against a newer schema (some migrations
  drop columns older code reads). To roll back across a migration,
  restore the pre-release snapshot - or ask the development team for a
  fix-forward instead.
- **Workflow code needs care.** Temporal replays a running workflow's
  history against the worker's code. If a release changes workflow logic
  (`internal/workflow/*_workflow.go`) while payments are in flight, the
  development team must flag it in the release notes, and either version
  the change or drain in-flight workflows first. Ask on every release:
  "does this change workflow code?"
- Shutdowns are graceful: both processes stop cleanly on `SIGTERM`
  (checked), so rolling deploys are safe. Give the API at least 35
  seconds to drain.

---

## 15. Backups and recovery

| Data | Back up? | Notes |
|---|---|---|
| Postgres `enquoir` | **Yes, with point-in-time recovery** | The record of every payment and account |
| Postgres `temporal`, `temporal_visibility` | Yes | In-flight workflows; losing them strands payments mid-way (recoverable by the team, but painful) |
| Redis | No | Cache and counters only |
| Images | Kept in your registry by SHA | Rollbacks |

A restore test before go-live is worth the hour.

---

## 16. Security checklist

- [ ] Temporal (7233) and its UI reachable only from the private network
- [ ] Postgres and Redis not publicly reachable (or locked to the services' addresses), with TLS where offered
- [ ] Secrets in the secret manager; none in images, repo or logs
- [ ] `SESSION_COOKIE_SECURE` unset; HTTPS only; HTTP redirects to HTTPS
- [ ] `CLIENT_IP_HEADER` set to a proxy-overwritten header and tested (section 9)
- [ ] `MAIL_FILE`, `ENQUOIR_CHAOS_KILL_AT` unset; `EMAIL_PROVIDER=resend`
- [ ] `AWX_BASE_URL` is the sandbox URL (the worker enforces it)
- [ ] Worker egress IP allowlisted at Airwallex; no other IPs added
- [ ] Postgres backups on; restore tested

---

## 17. Known limitations

- **Temporal Cloud** is not supported without a small code change (TLS
  or API-key options for the client).
- **Sandbox only.** Real payments are out of scope and blocked in code.
- **No dashboard yet.** Email links point at `https://enquoir.etimi.me`
  pages that don't exist until the frontend ships.
- **One worker replica recommended** because of Airwallex's 10-concurrent
  limit (section 11).
- **Webhooks from Airwallex are not used.** A transfer that fails *after*
  Airwallex reports it paid isn't detected.

---

## 18. Local reference

`deploy/docker-compose.yml` runs Postgres, Temporal (auto-setup), the
Temporal UI and Redis for development, all bound to `127.0.0.1`. It is a
faithful map of the dependencies, but **not** a production
configuration. `make up` starts it; `make api` and `make worker` run the
two services from source against it.

## Contacts

- Code, workflow changes, sandbox and email accounts: the project owner
  (Tee-Stark on GitHub).
- More detail: `README.md`, `docs/frontend.md` (what the dashboard
  needs), `docs/rail-notes.md` (Airwallex sandbox behaviour),
  `docs/progress.md` (history and decisions).
