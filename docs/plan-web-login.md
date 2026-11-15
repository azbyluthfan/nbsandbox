# Plan: signing in to the web UI

**Status (2026-11-15):** agreed (the open questions answered as proposed); being built.

## Today

The web UI is a development tool. It is off by default and listens on its own port (`127.0.0.1:8090`). Whoever opens it picks a user from `users.yaml` and acts as that user **without a password**: its calls go to `/ui/api/as/{user}/v1/...`, which runs the normal API as that user. Caddy never forwards it, and nbsandboxd warns when it listens beyond loopback.

## Goal

The same UI, usable by real users on a real deployment:
- They sign in with the identity provider that already issues API tokens (Entra ID, Keycloak, ...).
- They are the **same caller** in the UI as through the API: same caller ID (so the same spaces), same scopes.
- Revocation at the provider (sign-out, disabled account) ends the UI session too.

Nothing changes for API, gRPC or MCP callers. The development mode with the user picker stays, for a laptop.

## How it fits with OIDC

The UI listener becomes a small **backend for the browser** (the "BFF" pattern):

```
browser ──► /ui/login ──► provider sign-in page (authorization code + PKCE)
browser ◄── /ui/callback ◄── code
              nbsandboxd exchanges the code (with the UI client's secret)
              → access token for nbsandbox  ──► the existing Verifier: issuer, audience,
                                                 caller ID, scopes, introspection
              → refresh token, ID token     ──► kept server-side, encrypted
browser ◄── session cookie (random ID, HttpOnly; no token ever reaches the browser)

browser ── cookie ──► /ui/api/v1/... ──► session → Principal ──► the same API handlers
```

- **The identity comes from the access token**, checked by the same `Verifier` as API calls. So the caller ID (`kc:<sub>`, `entra:<tid>:<oid>`) and the scopes are exactly what that user's API token would give. The ID token is used only for the nonce check and a display name.
- **Tokens never reach the browser.** JavaScript sees a cookie it can't read and the display name. An XSS bug can act within the open page, but can't steal a token for use elsewhere.
- **`EventSource` works.** The live log and events streams send the cookie, which a bearer token in JavaScript couldn't do.
- **The main API never accepts the cookie.** `/v1` stays bearer or basic only, so it doesn't need CSRF protection.

## Design

### Modes

```yaml
ui:
  enabled: true
  listen: 127.0.0.1:8090
  auth: none            # none: the user picker (development; loopback only)
                        # oidc: sign in with the identity provider
                        # basic: a sign-in form against users.yaml
  public_url: https://nbsandbox.example.com   # required unless auth: none; the redirect URI is <public_url>/ui/callback
  session:
    idle: 1h            # signed out after this long without a request
    max: 12h            # and after this long in any case
  oidc:                 # issuer and audience come from auth.oidc
    client_id: nbsandbox-ui
    client_secret: ""   # NBSANDBOX_UI_OIDC_CLIENT_SECRET
    scopes: [openid, offline_access, nbsandbox.read, nbsandbox.spaces.write, nbsandbox.files.write, nbsandbox.runs]
```

- **`auth: none` is refused beyond loopback** (it is a warning today). That closes the "anyone who reaches the port is anyone" hole. This is a breaking change for anyone who opened the port on purpose; the CHANGELOG says how to move to `basic`.
- **`basic`**: a username/password form for small hosts without a provider. It uses the same bcrypt check and the same **login limiter** as the API (milestone 1).
- **`oidc` requires `auth.oidc`**: tokens are checked the same way, and the provider's configuration is shared.

### The sign-in flow (`oidc`)

1. `GET /ui/login?return=/spaces/x` creates a **login attempt** row: `state`, PKCE `code_verifier`, `nonce`, return path (relative paths only), expiring in 10 minutes. It redirects to the provider's `authorization_endpoint` with `response_type=code`, `code_challenge` (S256), `state`, `nonce` and the scopes.
2. `GET /ui/callback?code&state`: takes the attempt, using it once. It exchanges the code at the `token_endpoint` with the client secret and the verifier, checks the ID token's signature, audience and `nonce`, then verifies the access token with the API's `Verifier`. It creates the session, sets the cookie and redirects to the return path.
3. **Errors** (the user cancelled, the scopes were refused, a wrong audience) show a plain page with the reason and a "Try again" link. The details go to the log, as for refused API tokens.

Login attempts live in PostgreSQL, so the callback may reach another replica than the login did. No signing key is needed for a state cookie.

### Sessions

- **Table `ui_sessions`:**
  - `id_hash` (SHA-256 of the cookie value), `principal` (caller ID), `scopes`, `display_name`;
  - `access_token`, `refresh_token`, `id_token`, encrypted with AES-GCM;
  - `access_expires_at`, `created_at`, `last_seen_at`.
  - Cleanup removes expired rows with the existing cleanup loop.
- **The cookie:** `__Host-nbsandbox-ui` = 32 random bytes; `Secure`, `HttpOnly`, `SameSite=Lax`, `Path=/`, no `Expires` (it ends with the browser). Only its hash is stored, so a database read doesn't hand out sessions.
- **Encryption key:** derived from the UI client's secret with HKDF. No new secret to manage. Rotating the client secret signs everyone out, which is the right outcome after a leak. For `basic` there are no tokens to encrypt.
- **On each request:**
  1. look up the session; check `idle` and `max`;
  2. if the access token expires within a minute, **refresh** it at the provider; on failure (revoked, user disabled) the session ends and the UI shows "signed out";
  3. verify the access token with the `Verifier` (cached; with introspection on, revocation shows within its cache time);
  4. run the handler with that principal.
  - `last_seen_at` is written at most once a minute, not on every request.
- **Sign out:** `POST /ui/logout` deletes the row and, if the provider has an `end_session_endpoint`, redirects there with `id_token_hint` (RP-initiated logout). The user is then signed out of the provider too.

### CSRF and the page

- **`SameSite=Lax`** already keeps the cookie off cross-site POST, PUT and DELETE.
- **State-changing requests** also need `Origin` equal to `public_url` and a header `X-Nbsandbox-UI: 1`. A cross-site form can't set that header, and a cross-site `fetch` with it needs CORS, which the UI never allows.
- **Same CSP** as now (`script-src 'self'`, no inline script, nonce for CodeMirror's styles). `form-action` allows `'self'` only for the basic sign-in form.
- **Clickjacking:** `frame-ancestors 'none'` stays.

### Serving it in production

- **Same origin as the API.** Caddy forwards `/v1/*` and gRPC to the API port (unchanged) and everything else (`/`, `/ui/*`) to the UI port. One host name, one certificate, and the API's paths are unaffected. Because the API ignores cookies, sharing the origin adds no CSRF risk.
- **Compose:** the UI stays off in the production file. Enabling it means setting `NBSANDBOX_UI_AUTH` and `NBSANDBOX_PUBLIC_URL`; Caddy forwards to the UI only when it is on.
- **Helm:** `ui.enabled`, `ui.auth`, `ui.oidc.*` (secret from a Secret, like introspection), a Service port, and Ingress paths for `/` next to `/v1/`. The UI runs in the API Deployment.
- **Several API replicas** work: attempts and sessions are in PostgreSQL, and the key comes from the shared secret.

### The JavaScript

- `api.js`:
  - the base becomes `/ui/api/v1` (`/ui/api/as/{user}/v1` stays in `none` mode);
  - every write sends `X-Nbsandbox-UI: 1`;
  - a `401` sends the page to `/ui/login?return=<current path>`.
- **New:** `GET /ui/api/session` returns `{mode, user, display_name, scopes}`. The header shows the signed-in name and a "Sign out" button instead of the user picker.
- **Missing scopes** (for example no `nbsandbox.runs`) hide or disable the matching buttons; the API still enforces them.

### Provider setup (docs)

- **Keycloak:**
  - a confidential client `nbsandbox-ui` with the standard flow, the redirect URI `<public_url>/ui/callback`, PKCE S256 required;
  - the nbsandbox scopes as optional scopes, whose audience mapper gives `aud: nbsandbox`.
- **Entra ID:**
  - a web app registration with that redirect URI and a client secret;
  - API permissions for nbsandbox's delegated scopes, requested as `api://<nbsandbox client id>/nbsandbox.read` and so on;
  - Entra issues one resource's token per request, which is all the UI needs.

## Tests

- **Unit**, against the fake provider extended with authorize and token endpoints:
  - state, nonce and PKCE checked;
  - an attempt can't be used twice;
  - only relative return paths;
  - wrong audience or missing scopes refused;
  - refresh, and the session ending when refresh fails;
  - idle and max timeouts;
  - CSRF (`Origin`, header);
  - the cookie never accepted on `/v1`;
  - `none` refused beyond loopback;
  - the basic form is limited like the API.
- **Keycloak end to end** (`make test-keycloak`):
  - an HTTP client signs in through the real login page and gets a session;
  - lists its spaces and runs a notebook as `kc:<sub>`, the same owner as with a bearer token;
  - after sign-out at Keycloak, the next request is refused.
- **Browser check:** sign in, run, follow the live log, sign out (`docs/ui-check.md`).

## Order of work

1. Sessions and the middleware (store, cookie, CSRF), with `basic` mode first: testable without a provider.
2. The OIDC flow, refresh and logout; Keycloak e2e.
3. The JavaScript (session endpoint, header, 401 handling).
4. Caddy, Compose, Helm, docs (auth.md "Signing in to the web UI", deployment, design §6.7, threat model, CHANGELOG, ROADMAP).

## Open questions

1. **`auth: none` beyond loopback:** refuse to start (proposed), or keep the warning?
2. **`basic` mode in production:** offer it at all, or only `oidc` beyond loopback?
3. **Idle 1 h / max 12 h:** right defaults?
4. **Sign out of the provider too** on "Sign out" (proposed), or only end the nbsandbox session?
