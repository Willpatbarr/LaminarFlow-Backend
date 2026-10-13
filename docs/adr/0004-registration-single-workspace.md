# 4. Registration in a single-workspace model — join the workspace, no approval step yet

Status: Accepted — 2026-10-13 (LAM-61)

## Context

Nothing created an account. `internal/account` deleted them and `internal/auth` logged them
in, so the frontend had no way to get anyone a session. `POST /api/v1/auth/register` closes
that.

The schema is multi-tenant in shape and single-tenant in use. `workspace` (0003) exists so
the hosted offering can have many, but a self-hosted instance runs exactly one, and 0003
seeds it: every migrated database already contains a workspace called `Default` with nobody
in it. `workspace_member` (0020) is the join, with a `role` of `owner`, `admin` or `member`
that no code yet interprets.

That leaves two questions the ticket could not answer from the schema: what registration
does when a workspace already exists, and what it does when none does.

## Decision

### 1. A workspace exists: the new account joins it, as a member

Registration is unconditional. There is no invitation, no approval, no invite code and no
domain allowlist. The new account becomes a `member` of the one workspace and can read
everything in it.

If several workspaces exist (the test suite makes them; a future hosted deployment will), the
account joins the oldest, ordered by `created_at` then `id`. `me` reports the oldest
membership by the same rule, so the two cannot disagree.

### 2. No workspace exists: registration creates one

In the same transaction as the account and the membership, so there is never a moment at
which an account exists with nowhere to go. The workspace is named `Default`, matching the
row 0003 seeds.

In practice this branch is rare, because a migrated database always has the seeded row. The
common first sign-up is therefore case 1 against a workspace with no members, and it is
handled the same way: the account joins it.

### 3. The first member of a workspace is its `owner`

Everyone after is a `member`. The role means nothing yet, since no authority model reads it.
It is recorded now because who came first is unrecoverable later, and the alternative —
every row `member`, an owner assigned by hand when roles arrive — would make the first
person on every existing install indistinguishable from the thousandth.

A transaction-scoped advisory lock serialises registrations. Without it, two simultaneous
first sign-ups both see an empty `workspace` table and each create one: two workspaces, two
owners, and every later account joining only the older. The lock costs one uncontended
round trip in the ordinary case.

### 4. The duplicate-email response is not hidden

A registration endpoint cannot avoid telling the caller an address is taken: the caller needs
to know, and a "check your email" flow that hides it needs an email system, which does not
exist. `409 email_taken` is therefore an account-existence oracle, and this decision accepts
that.

What is controlled is everything *beyond* that:

- **Timing.** The password is hashed before the address is looked at, so a taken address pays
  the same bcrypt cost as a free one. The response time adds nothing the status does not.
- **Wording.** One code and one fixed message. The response says nothing about the existing
  account — not its name, not when it was created, not which workspace it is in.
- **Login stays blind.** `invalid_login` is still one answer for an unknown address and a
  wrong password (LAM-56). Registration reveals existence to someone *trying to register
  that address*; it does not make login a second place to ask.

### 5. Password policy

At least 10 characters (counted as characters, so a passphrase in any script is held to the
same bar) and at most 72 bytes. 72 is bcrypt's limit; past it the tail is ignored, and a
password whose tail does nothing is worse than one that is refused. Hashing is
`auth.HashPassword`, at `bcrypt.DefaultCost` — the cost `dummyHash` is built at, so login's
timing equalisation still holds for accounts created here.

## Consequences

- **Anyone who can reach this endpoint can join the workspace and read it.** There is no
  gate. That is acceptable only on a trusted network — a home server, a VPN, a private
  address — and **this must be revisited before the instance is exposed beyond one.**
  The smallest sufficient change is an invitation or approval step; the cheapest stopgap is
  a configuration switch that closes registration once the first account exists. Neither is
  built here, because choosing between them is a product decision.
- **No rate limiting.** Each registration costs a bcrypt hash and an unauthenticated caller
  can send them as fast as the network allows. Same exposure, same trigger to revisit.
- **Session creation is a second step.** If it fails after the transaction commits, the
  account exists and the caller sees a 500; logging in works. Making the two atomic would mean
  `internal/auth` writing inside `internal/account`'s transaction, for a failure that costs
  one extra request to recover from.
- **Write ownership.** `workspace` and `workspace_member` now belong to `internal/account`,
  guarded by `sqlguard` with `AnyReader` — everything joins `workspace_member` for scoping, so
  reads stay open and writes do not.
- **Permissions on `me` are the full set, always.** There is no authority model, so one user
  holds everything. The list is real (`auth.Permissions`) so roles narrow it later without
  changing the wire shape. An API token's `Scopes` are not applied to it, because nothing
  enforces them, and reporting a limit the API does not apply would be a lie the frontend
  would act on.

## Rejected

- **First account only; later ones refused.** The simplest way to close the door, and it
  turns a second teammate into an unrecoverable 403 on a product whose point is teams.
- **Every registration creates its own workspace.** Contradicts the single-workspace model:
  the new person would see an empty workspace and none of the work.
- **Hiding the duplicate behind a generic success.** Needs email to deliver the real outcome
  to the real owner; without it the person registering is simply told nothing and locked out.
