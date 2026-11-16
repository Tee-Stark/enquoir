# Enquoir demo video: brief for the video producer

This brief gives an AI (or a person) producing the Enquoir demo video
everything it needs to know: what the product is, how it works, what to
show on screen, and which claims are true. Use only the facts here. If
something isn't covered, leave it out rather than guessing.

---

## 1. The video at a glance

- **Product:** Enquoir, a safe control and observability layer for
  agentic finance.
- **Context:** a hackathon entry. The rail is Airwallex, **sandbox
  only**. No real money moves.
- **Audience:** hackathon judges and developers who build AI agents or
  payment systems. They are technical but have not seen the code.
- **Length:** 3 to 4 minutes.
- **Tone:** calm, precise, confident. Show it working; don't oversell.
  The product is about trust, so the video should feel trustworthy.
- **The one thing viewers should remember:** *an AI agent can retry as
  often as it likes, and the money still moves exactly once, with a
  record of every step.*

---

## 2. The problem (the hook)

AI agents are starting to move money through payment APIs. Models are
probabilistic. They:

- retry a request when they're unsure it worked,
- re-plan and send the same instruction again,
- lose context halfway through a task,
- make up a fresh request ID for a payment they already sent.

Each of these can turn one payment into two. Payment APIs give you
idempotency keys, but only if the caller reuses the same key, and a
model can't be trusted to.

**Enquoir sits between the agent and the payment rail and takes the
money-safety decisions away from the model.**

---

## 3. What Enquoir does, in plain words

The agent states an **intent**: "pay this payee this amount, for this
invoice, for this reason." Enquoir then:

1. **Derives a stable key** from the intent: a hash of the payee,
   amount, currency and reference. The same intent always gives the
   same key, however many times or however the agent sends it. The
   free-text reason is recorded but left out of the key, so an agent
   that rewords it on a retry still gets the same payment, not a new one.
2. **Checks policy.** Code, not the model, decides whether the payment
   may run. Today that's a maximum amount ($1,000,000 by default).
   Anything over it is `BLOCKED` before the rail is ever touched.
3. **Runs a durable workflow** (Temporal) that survives crashes and
   restarts.
4. **Writes before it calls.** Each step is recorded in Postgres before
   the matching request leaves for the bank rail.
5. **Never retries blind.** If a call's outcome is unclear (a timeout,
   a dropped connection, a crashed worker), Enquoir first looks the
   transfer up on the rail by its request ID. It retries only if the
   transfer is confirmed absent.
6. **Records a tamper-evident history.** Every event is hash-chained to
   the one before it, so any payment can be traced and checked.

### The three principles (good on-screen text)

1. **Every action has a policy.** Code decides whether a payment may run.
2. **Every execution has a history.** Each step is recorded before it acts.
3. **Every outcome can be investigated.** Any payment can be traced.

---

## 4. How it works (for the architecture diagram)

```
 AI agent
    │  POST /v1/intents  {payee, amount, currency, reference, reason}
    ▼
 Enquoir HTTP API (Go, chi)
    │  validates input, derives the intent key, checks the event log
    ▼
 Temporal workflow  (workflow ID = intent key)
    │  policy check → record → call → reconcile
    ├──► Postgres: hash-chained event log  (written BEFORE each call)
    ▼
 Airwallex sandbox  (request_id = intent key)
```

**The key idea to explain visually: one key, used in three places.**

| Where | What the key is used as | Why it stops duplicates |
|---|---|---|
| Enquoir's event log | the intent's ID | a resubmission sees the existing history and starts nothing new |
| Temporal | the workflow ID | two submissions attach to the same running workflow |
| Airwallex | the transfer's `request_id` | the rail recognises the same payment and doesn't create a second one |

### The lifecycle (for a state diagram)

```
PROPOSED → AUTHORIZED → IN_FLIGHT → APPLIED
    │           │            │
    └→ BLOCKED  └→ BLOCKED   ├→ UNKNOWN → (looked up, then APPLIED / FAILED / retried)
                             └→ FAILED
```

- `PROPOSED`: the intent was received.
- `AUTHORIZED`: policy passed.
- `IN_FLIGHT`: recorded, then the transfer request was sent.
- `UNKNOWN`: the outcome is unclear, so Enquoir looks it up before
  doing anything else.
- `APPLIED`: the rail confirmed the transfer is `PAID`.
- `FAILED`: the rail rejected or cancelled it, and the reason is
  recorded.
- `BLOCKED`: policy refused it. The rail was never called.

### Payees (beneficiaries)

Airwallex only pays a beneficiary it already holds, so payees are
registered first through `POST /v1/beneficiaries`. Airwallex's
beneficiary create has **no idempotency key**, so Enquoir supplies its
own: a hash of the payee's details is stored as the beneficiary's
nickname, and Enquoir always looks that up before creating anything.
Registering the same payee twice returns the same ID.

### Stack (one line on screen is enough)

Go · chi · Temporal · PostgreSQL (pgx) · Docker Compose · Airwallex
sandbox API.

---

## 5. Scene-by-scene script

Every on-screen step comes from `scripts/e2e.sh` (`make e2e`), which
prints each curl and its response. It has passed all 22 steps against
the real Airwallex sandbox. You can record one `make e2e` run, or run
the individual curls by hand for slower pacing.

**Before recording:** `make up`, `make sandbox-setup`, then `make worker`
and `make api` in two terminals. Run **only one** worker. Open the
Temporal UI at **http://localhost:8088**. Never show `.env` or the
sandbox token; the script already redacts the token.

### Scene 1: Hook (0:00 to 0:20)
- **Visual:** a chat-style mock of an agent: "Paying invoice INV-1001…
  timeout… retrying… paying invoice INV-1001."
- **Narration:** "AI agents are starting to move money. And agents
  retry. When a payment call times out, did it go through or not? Ask
  the model and you might pay twice."

### Scene 2: What Enquoir is (0:20 to 0:45)
- **Visual:** the architecture diagram from section 4, then the three
  principles.
- **Narration:** "Enquoir sits between the agent and the payment rail.
  The agent states an intent. Enquoir decides whether it may run,
  executes it durably, and records every step before it happens."

### Scene 3: Register a payee, twice (0:45 to 1:10)
- **Visual:** e2e steps 7 and 8. The same `POST /v1/beneficiaries` sent
  twice returns the same `id`.
- **Narration:** "First the payee. Airwallex has no idempotency key for
  creating beneficiaries, so Enquoir brings its own. Register the same
  payee twice and you get the same beneficiary."

### Scene 4: The agent pays, then retries (1:10 to 1:50)
- **Visual:** e2e step 11. The same intent is posted twice (the second with a reworded reason) and both
  responses carry the **same key**. Highlight the key.
- **Then:** e2e step 12. The rail is queried by `request_id` and holds
  **exactly one** transfer.
- **Narration:** "Now the agent pays $10.50 and, like agents do, sends
  it again. Both requests get the same key, because the key comes from
  the intent, not the model. On Airwallex there's exactly one transfer."

### Scene 5: Watch it in Temporal (1:50 to 2:20)
- **Visual:** the Temporal UI at http://localhost:8088. Open the
  workflow whose ID is the intent key and show the activity history:
  the policy check, record event, create transfer, poll.
- **Narration:** "Every payment is a durable Temporal workflow, and its
  ID is the intent key. If a worker crashes mid-payment, the workflow
  picks up where it left off, and it checks the rail before it retries
  anything."

### Scene 6: Settle to APPLIED (2:20 to 2:45)
- **Visual:** e2e steps 13 and 14. The sandbox simulator moves the
  transfer `SENT` → `PAID`, and the intent reaches `APPLIED`.
- **Narration:** "In the sandbox we simulate the bank moving the money.
  Once Airwallex reports it paid, the intent is applied."

### Scene 7: The audit trail (2:45 to 3:10)
- **Visual:** e2e step 16. The history is
  `PROPOSED → AUTHORIZED → IN_FLIGHT → APPLIED`, nothing repeated. Then
  step 15: resubmitting the paid intent just reports `APPLIED`.
- **Narration:** "Here's the full record, each event hash-chained to
  the last. Send the same intent tomorrow and Enquoir answers 'already
  applied.' Nothing new starts."

### Scene 8: Policy (3:10 to 3:25)
- **Visual:** e2e steps 9 and 10. A $2,000,000 intent ends `BLOCKED`,
  and its history is only `PROPOSED, BLOCKED`.
- **Narration:** "And code, not the model, decides what may run. Over
  the limit, it's blocked, and the rail is never called."

### Scene 9: Close (3:25 to 3:45)
- **Visual:** the product name and the takeaway line.
- **Narration:** "Agents can retry. Money moves once. Every step is on
  the record. That's Enquoir."

### Optional extra: the chaos test
`make chaos` kills a worker at four exact points in the money-moving
call (before recording, after recording but before the call, during
the call, after the call but before saving the result), restarts it,
and checks that there's still exactly one transfer. **It passed all four
points against the real sandbox.** The strongest moment to show is
`after_call_before_commit`: Airwallex had already accepted the transfer
when the worker died, before Enquoir could record it; the restarted
worker sent the create again with the same request ID, and Airwallex
still holds exactly one transfer. `during_call` is the same story one
step earlier: the worker died with the request sent and no answer read,
Airwallex created the transfer anyway, and the retry still made no
second one.

---

## 6. Claims: what you may and may not say

**True, and verified against the real Airwallex sandbox:**
- A full payment from intent to `APPLIED` (all 22 e2e steps pass).
- Submitting the same intent twice produces one key and one transfer on
  the rail.
- Registering the same payee twice returns the same beneficiary ID.
- Over-limit intents are `BLOCKED` without calling the rail.
- Resubmitting a finished intent starts nothing new.
- An agent that rewords the reason on a retry still gets the same key
  and one transfer.
- The chaos test: a worker killed at each of four points still ends
  with the payment applied and exactly one transfer. Including a worker
  killed after Airwallex accepted the transfer but before Enquoir
  recorded it: the retry reused the request ID and no second transfer
  was created.

**True, and verified by automated tests (not against the live sandbox):**
- When a call fails with an unclear outcome, Enquoir looks the transfer
  up by request ID before retrying, and sends no second create if it
  finds one.
- 20 concurrent duplicate submissions attach to one workflow.
- The event history is hash-chained and verifiable.

**Do not say:**
- That it handles real money or production traffic. It is sandbox only,
  by design, and refuses to start against any other Airwallex URL.
- That it supports many rails, currencies or countries. Today: US
  payees, USD, ACH, through Airwallex.
- That it has a full policy engine. Today it's a maximum-amount rule;
  the policy engine is future work.
- That it catches every failure after `PAID`. Airwallex says a `PAID`
  transfer can still fail later. Catching that needs webhooks, which
  are future work.

**Roadmap (fine to mention as "next"):** webhooks for after-the-fact
failures, a richer policy engine, a replayer for investigations, an
MCP proxy so agents connect directly, and more rails.

---

## 7. Glossary

- **Intent:** what the agent wants: payee, amount, currency, reference,
  reason.
- **Intent key:** a 32-character hash of the intent. The same intent
  always gives the same key.
- **Rail:** the payment provider that actually moves money (here,
  Airwallex).
- **`request_id`:** Airwallex's idempotency key. Enquoir sets it to the
  intent key.
- **Beneficiary:** a payee registered with Airwallex.
- **Temporal:** a durable workflow engine. Workflows survive crashes
  and resume.
- **Sandbox simulator:** Airwallex test endpoints that move a transfer
  through `SENT` and `PAID`, standing in for the bank.
- **Hash chain:** each event stores the hash of the one before it, so
  any tampering is detectable.

---

## 8. Practical notes for recording

- API: `http://localhost:8090`. Temporal UI: `http://localhost:8088`.
- Each `make e2e` run uses unique references, so you can run it again
  for another take.
- If a run hangs at "waiting for APPLIED", check that exactly one
  worker is running on current code.
- Keep the terminal font large, and dark-on-light or light-on-dark
  consistently. Highlight the intent key, the transfer count of 1, and
  the final event history.
