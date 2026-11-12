# Frontend guide

What a dashboard frontend needs to know to work with the Enquoir API.
The full contract is `api/openapi.yaml`, served at `/openapi.yaml` and
rendered at `/docs`; this page covers what the spec alone doesn't make
obvious.

## Where it runs

- The frontend is served at `APP_BASE_URL` (`https://enquoir.etimi.me`).
  Every link in Enquoir's emails points there; emails come from
  `no-reply@enquoir.etimi.me`.
- The API is a separate origin. List the frontend's origin in the API's
  `CORS_ALLOWED_ORIGINS`; a state-changing request a browser sends from
  any other origin is refused (`403 forbidden_origin`).
- Send requests with credentials (`fetch(url, { credentials: "include" })`)
  so the session cookie travels.

## Pages the emails link to

Each link carries a single-use token in `?token=`. The page reads it and
posts it to the API. Tokens are never shown again and expire.

| Path | Email | Post to | Body | Then |
|---|---|---|---|---|
| `/verify-email?token=…` | Confirm your email (24 h) | `POST /v1/auth/verify-email` | `{token}` | 204: show "verified". 400 `token_invalid`: offer "send a new link" (`POST /v1/auth/verify-email/resend`, signed in). |
| `/invitations/accept?token=…` | Invitation (7 days) | `POST /v1/invitations/accept` | `{token, password?}` | See below. |
| `/reset-password?token=…` | Reset password (1 h) | `POST /v1/auth/password/reset` | `{token, password}` | 204: send to sign-in. 400 `validation_failed`: the password is too weak, and the same link still works. 400 `token_invalid`: ask for a new link. |

**Accepting an invitation.** The page can't tell in advance whether the
address already has an account, so:

1. Post `{token}` alone.
2. If the answer is `200` with `account_created: false`, the person was
   added to the organisation. Send them to sign in.
3. If the answer is `400 validation_failed` with `field: "password"`, the
   address has no account yet, or only an unverified one. Ask them to
   choose a password and post `{token, password}`. A `200` sets the
   session cookie: they are signed in, already verified.
   `account_created: true` means a new account. `account_claimed: true`
   means an unverified account at that address was taken over by the
   link's holder: its old password no longer works and its old sessions
   are signed out. Worth saying so on the page ("we've secured the
   account at this address with your new password").

A weak password never spends the link, so the page can simply ask again.

## Sessions and organisations

- `POST /v1/auth/signup` and `POST /v1/auth/login` set the HttpOnly
  `enquoir_session` cookie. The frontend never sees the token.
- `GET /v1/auth/session` is the bootstrap call. It returns the user, the
  organisation the session is bound to, the user's `role` there, and
  `organisations`: every organisation they belong to, each with its role.
  A `401` means "show sign-in".
- One person can belong to several organisations, and a session is bound
  to one. To switch, call `POST /v1/auth/switch-organisation`
  `{organisation_id}`. It sets a new cookie; reload whatever you show.
- A role change or removal signs the member out of that organisation at
  once, so expect `401`s and send them back through sign-in.
- `GET /v1/auth/sessions` lists the user's sessions (`current` marks this
  one). `DELETE /v1/auth/sessions/{id}` and `DELETE /v1/auth/sessions`
  ("sign out everywhere else") revoke them.

## Roles: what to show

Use `role` from `GET /v1/auth/session` to hide what a member can't do.
The API enforces all of it either way (`403 forbidden`).

| | owner | admin | operator | viewer |
|---|---|---|---|---|
| Payments, attempts, replay, analytics, payees, agents, members, merchant integrations | ✓ | ✓ | ✓ | ✓ |
| Create, rotate, revoke agents; register payees | ✓ | ✓ | ✓ | |
| Invite and remove members, change roles; audit log | ✓ | ✓ | | |
| Create, disable, enable merchant integrations; issue and revoke their keys | ✓ | ✓ | | |
| Make or change admins and owners | ✓ | | | |

- In the role picker for invitations and role changes, offer only roles
  up to the member's own.
- Admins can't change or remove other admins, or owners.
- An organisation always keeps an owner. The last owner can't step down
  or leave (`409 last_owner`).
- Any member can leave: `DELETE /v1/members/{their own id}`.

## Unverified accounts

Until `user.email_verified` is true, a person can look around but can't
create or rotate agent keys, invite anyone, register payees, or create,
enable or re-key merchant integrations (`403 email_unverified`). Show a banner with "resend the link"
(`POST /v1/auth/verify-email/resend`; limited to 1 a minute, 5 a day).

## Merchant integrations (commerce)

A settings page for connecting merchants (`/v1/merchant-integrations`):
one integration per storefront, each with one key.

- Creating one, or rotating its key, returns the secret (`key.secret`)
  once. Show it with a copy button and a clear "you won't see this
  again"; afterwards only `key.prefix` is available.
- Statuses are `active` and `disabled`. Disabling stops the key at once;
  enabling lets it work again. Revoking the key (`DELETE .../key`)
  leaves an integration with `key: null` until a key is rotated in.
- Owners and admins manage integrations; operators and viewers only see
  them. Hide the buttons for them.
- Customers and agents are registered by the merchant's backend with its
  key, not from the dashboard; the dashboard has no view of them yet.
  The audit log shows their registration with an actor of type
  `merchant_integration`.

## The consent page (commerce)

Merchants' customers reach Enquoir only through this page, at
`APP_BASE_URL/consent?token=...`: the link Enquoir emails a customer when
a merchant asks for a mandate (permission for the merchant's AI agent to
buy for them). The customer has no Enquoir account; the token is the only
credential. Never put it anywhere but the request body.

1. On load, `POST /v1/consent/view` with `{"token": ...}`. Show every
   term in `terms`: the merchant and agent names, the payment method
   (`brand`, `last_four`), approval mode (`autonomous`: buys without
   asking; `per_transaction`: asks each time), the scope (amounts are
   minor units of `currency`; `max_transactions` null means no limit;
   empty `allowed_categories` means any) and `expires_at`. Viewing does
   not spend the link.
2. Approve: `POST /v1/consent/approve` with `{"token", "terms_hash"}`,
   where `terms_hash` is the one from step 1 - consent binds to exactly
   what was shown. Reject: `POST /v1/consent/reject` with `{"token"}`.
   Either spends the link; show a plain "done" page.
3. Errors: `400 token_invalid` - the link is spent, expired (1 hour), or
   the request was decided or expired: say so, and that the merchant can
   send a new request. `409 terms_mismatch` - reload and show the terms
   again.

The page's origin must be in the API's `CORS_ALLOWED_ORIGINS` (the
approve and reject calls are state-changing). After deciding, there is
nothing more for the customer to do at Enquoir: the merchant shows their
mandates, and asks to revoke them.

A revocation needs the customer's confirmation. The merchant asks; the
mandate pauses (`revocation_pending`) and Enquoir emails the customer a
link to `APP_BASE_URL/consent/revoke?token=...`. That page reads the
mandate with `POST /v1/consent/revocation/view` (`{"token"}`), shows what
is being revoked, and confirms with `POST /v1/consent/revocation/confirm`.
Same errors as above; unconfirmed, the request lapses after an hour.

## Limits

When a limit is hit, the answer is `429`, with `Retry-After` in seconds:

- `rate_limited`: too many attempts. Show "try again in N".
- `account_locked`: 10 failed log-ins lock the account for an hour.
  Suggest "reset your password", which also lifts the lock.

## Errors

Every error is RFC 9457 problem JSON. Branch on `code`, show `detail`,
and for `validation_failed` use `field` to point at the input. The codes
are listed in the spec's `Problem` schema.

## Lists

Lists are newest first and cursor-paged. Pass a response's `next_cursor`
as `cursor` for the next page; `null` means you're on the last page.
Times are RFC 3339. Money comes as `{minor_units, currency, amount}`:
use `amount` for display and `minor_units` for arithmetic, and never add
up amounts in different currencies.
