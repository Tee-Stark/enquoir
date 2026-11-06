# Airwallex sandbox: rail notes

What the real sandbox does, where it differs from the hackathon builder
guide (as summarised in AGENTS.md) or from our first assumptions. Each
entry was observed against `https://api.sandbox.airwallex.com` on
2026-10-23, or read from the Airwallex docs MCP server where noted.

## Differences found, and fixed

| Area | Guide / assumption | Real sandbox | Fixed in |
|---|---|---|---|
| Login | Success is 200 | **201 Created**, body `{"expires_at", "token"}` | `transport.ensureToken` accepts any 2xx |
| Beneficiary create | No idempotency key mentioned | **No `request_id`** at all. Enquoir sends its own key as `nickname`; `GET /beneficiaries?nick_name=` finds it (verified: a second `sandbox-setup` returned the same ID) | `airwallex/beneficiary.go`, `BeneficiaryWorkflow` |
| Beneficiary schema | `POST /beneficiaries/schema` | That endpoint returned errors. The live schema is `POST /beneficiary_form_schemas/generate` with `{account_currency, bank_country_code, entity_type, local_clearing_system, transfer_method}` | — |
| US ACH payee fields | Docs list most as optional | Schema **requires** `bank_account_category` (`Checking`/`Savings`; omitted gives `validation_failed` code 001), `address.state` as ISO 3166-2 (`US-WA`), postcode `^\d{5}(-\d{4})?$`, `local_clearing_system` `ACH` (not `LOCAL`) | `domain.NewBeneficiary`, adapter payload |
| Global accounts list | Item has a `currency` | **No top-level currency**; it is in `required_features[].currency` | `Simulator.ensureGlobalAccount` |
| Global account create | `{country_code, currency, transfer_method}` | Requires `request_id` and `required_features: [{currency, transfer_method}]` | same |
| Deposit simulation | `amount` as a string | `amount` is a JSON **number** (sent as `json.Number` from the exact decimal string, no float) | `Simulator.FundAccount` |
| Transfer transition | Body `{"status": ...}` | Body **`{"next_status": ...}`**; otherwise 400 `Missing body parameter "next_status"` | `Simulator.TransitionTransfer`, `scripts/e2e.sh` |
| New transfer status | Starts `SENT` | Starts **`PROCESSING`**; simulate `SENT`, then `PAID` | none needed: the workflow polls any non-final status |

## Observed on 2026-11-06 (second sandbox session)

| Area | Guide / assumption | Real sandbox |
|---|---|---|
| API key IP allowlist | Not mentioned | A login from an IP the key isn't allowlisted for gets **403 `{"code":"unauthorized","message":"Forbidden"}`**, exactly the response a made-up key gets. When login suddenly starts failing, check the allowlist before the keys. Once, mid-session, the same key gave **401 `credentials_invalid`** instead. |
| `duplicate_request_id` | Shape unknown | **HTTP 400**, `{"code":"duplicate_request_id","details":{"id":"<existing transfer id>","request_id":"..."},"message":...,"source":"request_id"}`. Observed by re-sending the exact create of a transfer that already existed: no second transfer. The adapter's match on `code`, then lookup, handles it; `details.id` is unused. |
| LOCAL transfer fee | "LOCAL payouts have no fee" | A US/USD/LOCAL transfer of 7.25 came back with **`fee_amount: 3`, `fee_paid_by: PAYER`**, `amount_payer_pays: 10.25`. Not free in this sandbox; worth knowing when funding runs. |
| Simulator transitions | Each call applies | Sending `PAID` immediately after `SENT` once did **not** apply (the transfer stayed `SENT`); repeating `SENT` on a `SENT` transfer returns **500 `operation_failed`**. `cmd/chaos` now confirms each transition by reading the transfer back and retries. |

## Confirmed as documented

- `POST /transfers/create` with `request_id` = the intent key (32 hex
  chars), `beneficiary_id`, `transfer_currency`/`source_currency`
  `USD`, `transfer_amount` as a major-unit string, `transfer_method`
  `LOCAL`, `reason` `other`: accepted.
- `GET /transfers?request_id=` returns the transfer, and items carry
  `request_id`. Exactly one transfer per intent key across every run.
- Transfer amounts come back as numbers (`"transfer_amount": 10.5`).

## Statuses (from the docs' status table)

`PROCESSING → SENT → PAID`, with `FAILED` possible from any of those.
`FAILED` "will be automatically cancelled in seconds", so a failure
still ends as `CANCELLED`, which the workflow treats as final.

**Known gap:** the docs say a `PAID` transfer "may subsequently fail"
if the clearing system or recipient bank rejects it. The workflow
records `APPLIED` once it sees `PAID` and stops watching. Catching a
later failure needs webhooks (`payout.transfer.failed`), which are out
of scope for now.

## Not yet observed

- Whether `nick_name` filtering is exact or a substring match. The
  adapter only accepts an exact match either way.
- A create whose request was genuinely in the air when the worker died
  (see `during_call` in docs/progress.md: its kill fires before the
  request leaves).
