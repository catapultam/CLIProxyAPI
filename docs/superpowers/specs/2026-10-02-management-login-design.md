# Management panel login: username/password and passkeys

Date: 2026-10-02
Status: draft, awaiting user review

## Goal

Replace the "paste the management key" prompt in the management panel with a normal login: username and password, plus one-tap passkey sign-in. The point is convenience. The panel is reachable only on the user's tailnet, so the design favors "log in once per device and forget about it" over hardening.

## Scope and decisions (agreed with the user)

- Single admin account. No roles, no user management.
- The account is created inside the panel after one login with the existing management key. The management key remains valid everywhere (API, TUI, scripts, RESP `AUTH`, `/v0`) and is the break-glass recovery path.
- Sessions: an HttpOnly cookie for the same-origin panel, plus a bearer session token fallback when the panel talks to a different origin.
- The panel UI lives in the separate repo `Cli-Proxy-API-Management-Center` (CPAMC). This spec covers both sides.

## Deployment facts this depends on (cakebox, from the comms session)

- The panel is used at `http://cakebox.wyrm-cat.ts.net:8318`, an nginx container that proxies the API. It is HTTP only.
- `https://cakebox.wyrm-cat.ts.net/` is a `tailscale serve` HTTPS proxy straight to the proxy on `:8317`, which also serves `/management.html`. Tailnet HTTPS certificates are enabled.
- Passkeys (WebAuthn) require a secure origin. They therefore work on the HTTPS name and not on the `:8318` HTTP origin. Password login works on both.

## User experience

1. **No account yet.** The panel shows the existing management key prompt. After login, a banner says "Set up a username and password" and links to the account page.
2. **Account page** (in the panel's settings/system area):
   - Set or change the username and password.
   - "Add a passkey" (shown only when the page is on a passkey-enabled origin).
   - List, rename and delete passkeys.
   - "Sign out all devices".
3. **Login screen once an account exists:**
   - Username and password fields.
   - A "Sign in with passkey" button when the origin supports it. It uses discoverable credentials, so there is no username typing.
   - A small "Use management key instead" link.
   - The API-base field stays, collapsed under "Advanced", for pointing the panel at another server.
4. **Staying logged in.** A session lasts 30 days and slides forward on every use, so a device that opens the panel at least monthly never sees the login screen again. It survives proxy restarts and redeploys.

## Backend design (CLIProxyAPI)

### Storage

The account lives in `config.yaml` under the existing `management:` block, next to `secret-key`. It therefore persists through every config backend (file, Postgres, git, object store) with no new storage code, and like `secret-key` it is excluded from the JSON config view (`RemoteManagement` is `json:"-"`).

```yaml
management:
  login:
    username: admin
    password-hash: "$argon2id$v=19$m=65536,t=3,p=4$..."   # plaintext is hashed on load, like secret-key
    session-secret: "<base64, auto-generated>"            # signs session cookies/tokens
    passkey-rp-id: cakebox.wyrm-cat.ts.net                # empty = passkeys disabled
    passkey-origins: ["https://cakebox.wyrm-cat.ts.net"]
    passkeys:
      - id: "<base64url credential id>"
        public-key: "<base64url COSE key>"
        name: "Pixel 9"
        created: 2026-10-02T21:00:00Z
```

- **Password hashing:** use argon2id (`golang.org/x/crypto/argon2`, already a dependency). A plaintext `password` value written by hand is hashed on load and written back. This is the same mechanism `secret-key` already uses in `config_load.go`.
- **Passkey sign counters are not persisted.** Synced passkeys (iCloud, Google, 1Password) always report 0, and persisting the counter would rewrite `config.yaml` on every login.
- **`session-secret`** is generated the first time an account is created. "Sign out all devices", a password change and deleting a passkey all regenerate it, which invalidates every existing session.

### Sessions

- **Token format:** a stateless signed token, `cpas_<base64url(payload)>.<base64url(HMAC-SHA256)>`.
  - The payload holds the issued-at time, the expiry, and the login method (password, passkey or key).
  - The HMAC key is `session-secret`.
  - Because the token is stateless, no server-side store is needed and restarts don't log anyone out.
- **Lifetime:** 30 days, sliding. When less than half the lifetime remains, the middleware reissues the token: it sets a fresh cookie, or returns the new value in the `X-CPA-Session-Refresh` header for bearer clients.
- **Cookie:**
  - Name `cpa_mgmt_session`, attributes `HttpOnly`, `SameSite=Strict`, `Path=/`.
  - `Secure` is set when the request is HTTPS, either direct TLS or `X-Forwarded-Proto: https` from a trusted proxy.
- **Bearer fallback:** login returns the same token in the response body. The panel uses it as `Authorization: Bearer cpas_...` when its API base is cross-origin. Same-origin panels ignore the body value and rely on the cookie.
- **CSRF guard:** applies only to cookie-authenticated requests whose method is not GET, HEAD or OPTIONS.
  - The request must carry `Sec-Fetch-Site: same-origin`.
  - If that header is absent, `Origin` must match the request host (or `X-Forwarded-Host` from a trusted proxy, since the nginx panel container rewrites `Host`) or a configured passkey origin.
  - Bearer-authenticated requests are not cookie-driven, so they need no CSRF check.

### Middleware change

`Middleware()` in `internal/api/handlers/management/handler.go` gets one new first step:

1. If there is a `cpa_mgmt_session` cookie or an `Authorization: Bearer cpas_...` header that verifies, the request is authenticated (subject to the CSRF guard).
2. Otherwise fall through to the existing management-key logic, unchanged.

Session auth bypasses the `allow-remote` check, because the session itself is the proof. Key-based access, rate limiting and `/v0` handler code stay as they are.

### Routes

Public, under `/v8/management/session/`, with no key required:

| Route | Purpose |
|---|---|
| `GET status` | `{account: bool, passkeys: bool (for this request's origin), authenticated: bool, username}` |
| `POST login` | `{username, password}`; on success sets the cookie and returns `{token, expires}` |
| `POST passkey/begin` | Returns WebAuthn assertion options (discoverable, no username) |
| `POST passkey/finish` | Verifies the assertion; sets the cookie and returns `{token, expires}` |
| `POST logout` | Clears the cookie |

Authenticated (key or session), under `/v8/management/account`:

| Route | Purpose |
|---|---|
| `GET` | Username and passkey list (no secrets) |
| `PUT` | Set or change the username and password. Changing an existing password over a session requires the current password; over the key it does not, which is the recovery path |
| `POST passkeys/begin`, `POST passkeys/finish` | Register a passkey |
| `PATCH passkeys/:id`, `DELETE passkeys/:id` | Rename or delete a passkey |
| `POST sign-out-all` | Regenerate `session-secret` |

- **Passkey library:** `github.com/go-webauthn/webauthn` (new dependency).
- **Ceremony challenges:** kept in memory with a 5-minute TTL and are single-use. A restart in the middle of a ceremony just means pressing the button again.

### Login throttling

Every client on the tailnet TCP forwards reaches the proxy as localhost or the Docker gateway, so per-IP bans would lock out the admin along with an attacker. Instead:

- Failed password logins add a delay to the next attempt (1s, 2s, 4s… capped at 30s).
- The counter is global to the single account and resets on success.
- There is no ban.
- Passkey logins are not throttled; they cannot be guessed.

### Code layout

- **New package `internal/mgmtauth/`:**
  - `password.go`: argon2id.
  - `token.go`: signing and verification, with an injectable clock for tests.
  - `passkey.go`: go-webauthn wrapper plus the challenge cache.
  - `throttle.go`.
- **Handlers:** live in `internal/api/handlers/management/` as `session.go` and `account.go`, next to the existing management handlers.
- **Config:** `internal/config` gains the `Login` struct inside `RemoteManagement`, its load-time hashing and `config.example.yaml` docs.

### Route availability

Today management routes are registered only when a `secret-key`, `MANAGEMENT_PASSWORD` or local password exists. That stays the rule, so creating an account always requires a key first. A configured login account also counts as "management enabled" for hot-reload purposes.

## Panel design (CPAMC repo)

- **`useAuthStore`:**
  - Gains a session mode alongside key mode.
  - `restoreSession` first calls `GET session/status`; an authenticated response means it is logged in with no stored secret.
  - In key mode it behaves as it does today.
- **`apiClient`:**
  - Same origin: sends cookies (axios default) and no Authorization header.
  - Cross-origin: sends `Authorization: Bearer cpas_...`, stored where the key is stored today, and picks up `X-CPA-Session-Refresh`.
- **`LoginPage.tsx`:** the layout described under User experience. Passkeys use `navigator.credentials.get` with options from `passkey/begin`.
- **New account page:** password form, passkey list with add/rename/delete, and "Sign out all devices".
- **Logout** calls `POST session/logout` and then clears local state.
- **Rebase:** the user's `ui-refresh` branch in CPAMC is uncommitted and in progress. The panel work must be based on it (or wait for it to land) to avoid conflicts.

## Open deployment choice (needs the user's approval before anyone touches cakebox)

Passkeys need the panel on HTTPS. There are two options:

1. **Open the panel at `https://cakebox.wyrm-cat.ts.net/management.html`.** This works today. That page is downloaded from `panel-github-repository`, so that setting must point at a release built from the user's CPAMC fork.
2. **Add an HTTPS `tailscale serve` for the nginx panel container**, for example `https://cakebox.wyrm-cat.ts.net:8443 -> 127.0.0.1:8318`. Then add that origin to `passkey-origins`.

Passkeys registered on either origin work on both, because the RP ID is the hostname.

## Testing

- **`mgmtauth` unit tests:**
  - Password hash and verify round-trip.
  - Token sign/verify, expiry, sliding refresh and secret rotation, all with a mock clock and no `time.Sleep`.
  - Throttle delays.
  - Challenge single-use and TTL.
- **Handler tests:**
  - Login success and failure.
  - Cookie attributes (Secure only on HTTPS).
  - The CSRF guard rejects a cross-site POST that carries a valid cookie.
  - A bearer token works cross-origin.
  - Key auth is unchanged.
  - A session reaches `/v8` and `/v0` routes.
  - Account PUT needs the current password over a session but not over the key.
  - Sign-out-all invalidates old tokens.
- **Passkey tests:**
  - Registration and login against the go-webauthn test helpers / a virtual authenticator fixture.
  - An origin outside `passkey-origins` is rejected.
- **Config tests:** a plaintext password is hashed on load and written back; the login block is absent from the JSON config view.
- **Panel:** store and API-client unit tests for session mode, and a manual pass on both cakebox origins.

## Out of scope / noted separately

- **Possible localhost-trust bypass on the TCP forwards.** `allow-remote` and the local-password path trust `ClientIP() == 127.0.0.1`, and the tailnet TCP forwards on `:8317`/`:8327` may make every tailnet client look local. This exists today and is unrelated to the login feature. It is worth verifying separately.
- Multiple users, roles, TOTP, email recovery.
