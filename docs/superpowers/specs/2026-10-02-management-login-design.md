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

The account lives in a sidecar file, `management-login.dat`. The content is JSON, but the name is deliberately not `*.json`, so the auth-file endpoints and watcher never treat it as a credential. The file has mode 0600 and is written atomically (temp file plus rename).

Its location is resolved in this order:
1. The full path in env `MANAGEMENT_LOGIN_FILE`.
2. `WRITABLE_PATH`.
3. The auth dir, which is persistent in the default docker-compose.
4. The config file's directory.

On cakebox this resolves to `/data/management-login.dat`.

A corrupt or unreadable file puts the store into a "broken" state. Login is unavailable and writes are refused (503) so the file is never overwritten, while the key keeps working.

**Revised after review.** The first version stored the account in `config.yaml`, which caused these problems:
- The v8 config endpoints build their responses from the raw YAML, so the session secret and password hash leaked.
- Generic config writers could change the password without the account rules.
- A stale config save resurrected revoked secrets and deleted passkeys.
- Emptied fields could never be deleted.
- Hot reloads raced with account changes.

With the sidecar, only the `/account` API reads or writes the account. Config endpoints never see it.

```json
{
  "username": "admin",
  "password_hash": "$argon2id$v=19$m=65536,t=3,p=4$...",
  "session_secret": "<base64, generated on first setup>",
  "user_handle": "<base64url, 32 random bytes>",
  "passkey_rp_id": "cakebox.wyrm-cat.ts.net",
  "passkey_origins": ["https://cakebox.wyrm-cat.ts.net:8443"],
  "passkeys": [{"id": "...", "rp_id": "...", "public_key": "...", "attestation_type": "none", "transports": [], "aaguid": "...", "backup_eligible": true, "backup_state": true, "name": "Pixel 9", "created_at": "..."}]
}
```

- **Loading and mutation:** the file is loaded once at startup into an in-memory store with its own lock. Every mutation deep-copies, validates, persists, then publishes, so a failed write leaves the live state unchanged. A missing file means there is no account. A corrupt file is logged and treated as no account; the management key still works.
- **No hand editing.** There are no config fields for the account; recovery is the management key. Hand edits to the sidecar need a restart.
- **Other storage backends:** the sidecar is not synced to the Postgres, git or object stores. If it is lost, the account is set up again via the key. On cakebox it sits on the persistent `/data` mount.
- **Stored credential fields:** everything go-webauthn's `webauthn.Credential` needs to validate a later assertion (notably the backup-eligible/backup-state flags), plus the rp-id the passkey was registered under. The sign counter is not stored. `passkeys_available` counts only passkeys for the current rp-id.
- **Password hashing:** argon2id (`golang.org/x/crypto/argon2`, already a dependency).
- **Passkey sign counters are not persisted.** Synced passkeys always report 0, and persisting the counter would mean a file write on every login.
- **`session-secret`** is generated the first time an account is created. "Sign out all devices" and a password change regenerate it, which invalidates every existing session. On a password change the caller immediately gets a fresh session (cookie + token in the response) so they stay logged in.

### Sessions

- **Token format:** a stateless signed token, `cpas_<base64url(payload)>.<base64url(HMAC-SHA256)>`.
  - The payload holds the issued-at time, the expiry, and the login method (password, passkey or key).
  - The HMAC key is `session-secret`.
  - Because the token is stateless, no server-side store is needed and restarts don't log anyone out.
- **Lifetime:** 30 days, sliding. When less than half the lifetime remains, the middleware reissues the token: it sets a fresh cookie, or returns the new value in the `X-CPA-Session-Refresh` header for bearer clients.
- **Cookie:**
  - Name `cpa_mgmt_session`, attributes `HttpOnly`, `SameSite=Strict`, `Path=/v8/management`. The panel only calls v8, and the narrower path keeps the cookie off ordinary proxy requests and out of their request logs. `Cookie`/`Set-Cookie` are also masked in request logging.
  - `Secure` is set when the request is HTTPS: direct TLS, `X-Forwarded-Proto: https` from a trusted proxy, or an `Origin` header with the `https` scheme. The last case matters on cakebox, where `tailscale serve` terminates TLS in front of an nginx that forwards `X-Forwarded-Proto: http`.
  - Max-Age matches the token expiry.
- **Bearer fallback:** login returns the same token in the response body. The panel uses it as `Authorization: Bearer cpas_...` when its API base is cross-origin. Same-origin panels ignore the body value and rely on the cookie.
- **CSRF guard:** applies only to cookie-authenticated requests whose method is not GET, HEAD or OPTIONS.
  - The request must carry `Sec-Fetch-Site: same-origin`.
  - If that header is absent, the `Origin` host must equal the request `Host` (the nginx panel container preserves `Host`), or `Origin` must be one of the configured passkey origins. A request with neither header is rejected.
  - Bearer-authenticated requests are not cookie-driven, so they need no CSRF check.

### Middleware change

`Middleware()` in `internal/api/handlers/management/handler.go` gets one new first step:

1. If there is a `cpa_mgmt_session` cookie or an `Authorization: Bearer cpas_...` header that verifies, the request is authenticated (subject to the CSRF guard).
2. Otherwise fall through to the existing management-key logic, unchanged.

Session auth obeys the same remote predicate as key auth (`local || allow-remote`, including the `MANAGEMENT_PASSWORD` override), so `allow-remote: false` keeps its meaning. A stale session credential sent alongside a valid management key falls through to key auth without counting a failure. `GET /session/status` never counts key failures. Key-based access, rate limiting and `/v0` handler code stay as they are.

### Routes

This is the exact contract the panel codes against. All bodies are JSON. Errors are `{"error": "<message>"}` with a meaningful status. A "session response" means: set the `cpa_mgmt_session` cookie and return `{"token": "cpas_...", "expires_at": "<RFC3339>"}`.

Public, under `/v8/management/session/`. No key is required, but they still return 404 when management is unavailable (same availability rule as the rest of `/v8/management`):

| Route | Request | Response |
|---|---|---|
| `GET status` | none | `200 {"account": bool, "authenticated": bool, "method": "password"\|"passkey"\|"key"\|"", "passkeys_available": bool, "passkey_origins": [..]}`. `account` = a username and password hash exist. `authenticated` = a valid session cookie/bearer token, or a valid management key, came with the request. `passkeys_available` = rp-id set and at least one passkey registered. |
| `POST login` | `{"username", "password"}` | `200` session response. `401` wrong credentials, `409` no account, `429 {"error", "retry_after": seconds}` when throttled |
| `POST passkey/begin` | none | `200 {"ceremony_id": "...", "options": <go-webauthn protocol.CredentialAssertion JSON, i.e. {"publicKey": {...}}>}`. Discoverable login: no allowCredentials. `409` when passkeys are unavailable |
| `POST passkey/finish` | `{"ceremony_id", "credential": <PublicKeyCredential JSON as produced by credential.toJSON()>}` | `200` session response, `401` on failure. The ceremony is single-use either way |
| `POST logout` | none | `204`; clears the cookie (Max-Age=0) |

Authenticated (key or session), under `/v8/management/account`:

| Route | Request | Response |
|---|---|---|
| `GET /account` | none | `200 {"configured": bool, "username": "", "passkeys": [{"id", "name", "created_at"}], "passkey_rp_id": "", "passkey_origins": []}` |
| `PUT /account` | `{"username", "password", "current_password"?}` | `200` session response for the caller. On first setup it generates `session-secret` and `user-handle`, and the password is required. On an existing account an empty `password` keeps the current one, so a username-only change is valid. Over a session, any change to an existing account requires the correct `current_password` (`403` otherwise); over the management key it is never required. `session-secret` rotates only when the password changes. `400` for policy failures (password < 12 chars, empty username) |
| `PUT /account/passkey-settings` | `{"rp_id", "origins": [], "current_password"?}` | `200` updated account view. Lets the panel configure passkeys; the panel pre-fills from `window.location`. Over a session `current_password` is required (`403` otherwise) |
| `POST /account/passkeys/begin` | `{"current_password"?}` | `200 {"ceremony_id", "options": <protocol.CredentialCreation JSON>}`. Uses resident key = required, user verification = required, and excludeCredentials = existing passkeys. Over a session `current_password` is required (`403` otherwise), so a stolen session cannot plant a persistent passkey. `409` when rp-id is not set or no account exists |
| `POST /account/passkeys/finish` | `{"ceremony_id", "name", "credential": <toJSON() output>}` | `200 {"id", "name", "created_at"}` |
| `PATCH /account/passkeys/:id` | `{"name"}` | `200` the passkey |
| `DELETE /account/passkeys/:id` | none | `204` |
| `POST /account/sign-out-all` | none | `204`; rotates `session-secret` and clears the caller's cookie |

These routes persist through the same path the other management config writers use: mutate the config under the handler's lock, then save with comment preservation. The existing watcher/store sync then takes over.

The origin check: WebAuthn verification must accept every origin in `passkey-origins`. If that list is empty, it defaults to `https://<rp-id>`. `GET session/status` and `GET /account` return this effective list, and `session/status` also returns `passkey_rp_id`.

Other contract details:
- `retry_after` is rounded up to whole seconds and is at least 1.
- The CORS middleware adds `X-CPA-Session-Refresh` to `Access-Control-Expose-Headers` so cross-origin bearer clients can read it. ACAO stays `*` with no Allow-Credentials, so clients must never send credentialed (`withCredentials`) requests.
- Because tokens are stateless, logout clears the cookie but cannot revoke a copied bearer token; "Sign out all devices" is the revocation path.

- **Passkey library:** `github.com/go-webauthn/webauthn` (new dependency).
- **Ceremony challenges:** kept in memory with a 5-minute TTL and are single-use. A restart in the middle of a ceremony just means pressing the button again.

### Login throttling

Every client on the tailnet TCP forwards reaches the proxy as localhost or the Docker gateway, so per-IP bans would lock out the admin along with an attacker. Instead:

- Failed password logins open a wait window before the next attempt is accepted (1s, 2s, 4s… capped at 30s). Attempts inside the window get `429` with `retry_after`; the handler never sleeps.
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
- **Storage:** the sidecar account store lives in `internal/mgmtauth`. `internal/config` is unchanged apart from a short comment in `config.example.yaml`.

### Route availability

Today management routes are registered only when a `secret-key`, `MANAGEMENT_PASSWORD` or local password exists. That stays the rule, so creating an account always requires a key first. A login account on its own never enables management.

Hardening added after review:
- Password verification runs one at a time; concurrent attempts get `429`.
- At most 256 passkey ceremonies can be pending.
- A stale `cpas_` credential returns `401 session expired` and never counts toward the per-IP key ban.
- `/session/login` and `/session/passkey/finish` obey the same `allow-remote` predicate as key auth.
- For cookie-authenticated requests of any method, `Sec-Fetch-Site: same-site` or `cross-site` is rejected.
- Sessions require an existing account.

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

## Deployment (decided)

The user said to make the call. The choice is to add an HTTPS `tailscale serve` for the existing nginx panel container: `https://cakebox.wyrm-cat.ts.net:8443 -> http://127.0.0.1:8318`.

- This keeps the user's own panel build. The proxy-served `/management.html` on 443 is the upstream release.
- It leaves the existing HTTP `:8318` URL working for password login.
- Cakebox config sets `passkey-rp-id: cakebox.wyrm-cat.ts.net` and `passkey-origins: ["https://cakebox.wyrm-cat.ts.net:8443"]`.

Because the RP ID is the hostname, passkeys would also work on any other HTTPS origin on that host that gets added to the list later.

Panel cookie vs bearer mode: the panel uses cookie mode when its API base has the same origin as `window.location`, and bearer mode otherwise.

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
