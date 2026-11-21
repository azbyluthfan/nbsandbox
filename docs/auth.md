# Authentication

nbsandbox accepts two kinds of credentials on every `/v1` endpoint:

- **Bearer access tokens** from your identity provider (OpenID Connect). nbsandbox is an OAuth 2.0 *resource server*: it checks tokens, it never issues them, and it keeps no user accounts.
- **Basic auth** against `users.yaml`, for development and single-host use. On by default; turn it off in production once tokens work.

## How a token is checked

```
your app ──(user signs in)──► identity provider ──► access token for nbsandbox
your app ── Authorization: Bearer <token> ──► nbsandboxd
   1. signature: a key from the provider's published key set (RS*, PS* or ES* only; never "none" or HMAC)
   2. iss is exactly the configured issuer; aud includes nbsandbox's audience
   3. exp / nbf / iat within 60 s of clock leeway; lifetime at most max_lifetime (24 h)
   4. caller ID = <id_prefix>:<id claims>   e.g. entra:<tid>:<oid>
   5. scopes from scp, scope and roles → what the caller may do
   6. with introspection on: the provider says the token is still active (cached 30 s)
```

The provider's keys are fetched from its discovery document (or `jwks_url`), cached, refreshed hourly, and refetched when a token names an unknown key (at most once a minute). If the provider is unreachable, keys already loaded keep working. Tokens are never logged; refused tokens are logged with the reason, and the caller only gets `401` with `WWW-Authenticate: Bearer error="invalid_token"`.

**Spaces belong to the caller ID.** There is no user table: a caller who presents a valid token can create spaces and use their own. Removing someone at the provider ends their access as soon as their tokens expire, so keep access tokens short-lived (5–15 minutes), or turn on [revocation](#revoked-tokens) to end it within seconds. Basic-auth users are their username (`alice`); a token caller is always `<prefix>:...`, so the two can never collide.

## Scopes

Each endpoint needs one scope (listed per operation in [api/openapi.yaml](../api/openapi.yaml)). Without it: `403`, code `forbidden`, and `WWW-Authenticate: Bearer error="insufficient_scope", scope="..."`. Basic-auth users hold every scope except `nbsandbox.admin`, which a basic user gets only with `admin: true` in users.yaml.

| Scope | Allows |
|---|---|
| `nbsandbox.read` | list and read spaces, files, runs, results, logs and streams, and sessions; the runtime |
| `nbsandbox.spaces.write` | create, rename and delete spaces |
| `nbsandbox.files.write` | upload, edit, move and delete files and folders; promote artifacts |
| `nbsandbox.runs` | start, cancel and delete runs; open, use and end interactive sessions (a session runs code, as a run does) |
| `nbsandbox.functions.invoke` | call functions |
| `nbsandbox.functions.publish` | publish and delete functions |
| `nbsandbox.admin` | every owner's spaces: list them, give one to another owner, delete one (`/v1/admin/spaces`, gRPC `AdminService`); every owner's runs and sessions, stopping them; draining; the audit log. Grant it only to operators: it is never implied by the others |

A backend that only calls functions needs `nbsandbox.functions.invoke nbsandbox.read` (read lets it fetch results and follow status).

Scopes are read from `scp` (Entra ID delegated permissions, space-separated), `scope` (RFC 9068, space-separated) and `roles` (Entra ID app roles, a list). Give scopes exactly these names at your provider.

## Calling on behalf of a user

Your backend holds its users' access tokens, but those tokens were issued **for your backend** (their `aud` is your backend), and nbsandbox refuses them: otherwise any API that receives a user's token could replay it against nbsandbox. The backend exchanges the user's token for one issued to nbsandbox, then calls nbsandbox with it. nbsandbox sees the end user's ID (their spaces), and records the calling app (`azp`).

- **Entra ID:** the On-Behalf-Of flow: `POST https://login.microsoftonline.com/<tenant>/oauth2/v2.0/token` with `grant_type=urn:ietf:params:oauth:grant-type:jwt-bearer`, `requested_token_use=on_behalf_of`, `assertion=<the user's token>`, `scope=api://<nbsandbox client id>/nbsandbox.runs ...`, and your backend's client credentials.
- **Keycloak, Okta, Auth0 and others:** OAuth 2.0 Token Exchange (RFC 8693): `grant_type=urn:ietf:params:oauth:grant-type:token-exchange`, `subject_token=<the user's token>`, `audience=<nbsandbox's audience>`.

Cache the exchanged token until shortly before it expires; don't exchange on every call. See [examples/](../examples/) for a complete client.

## Configuring

In `settings.yaml`:

```yaml
auth:
  basic: true                 # users.yaml; set false in production once tokens work
  oidc:
    issuer: https://login.microsoftonline.com/<tenant id>/v2.0
    audience: <nbsandbox's client id>
    id_prefix: entra          # stored in every space's owner: never change it
    id_claims: [tid, oid]     # stable, never reused; never email or upn
    # jwks_url: ""            # default: from <issuer>/.well-known/openid-configuration
    # leeway: 60s             # at most 5m
    # max_lifetime: 24h
    # keys_refresh: 1h
```

Or with environment variables: `NBSANDBOX_AUTH_BASIC=false`, `NBSANDBOX_OIDC_ISSUER`, `NBSANDBOX_OIDC_AUDIENCE`, `NBSANDBOX_OIDC_JWKS_URL`, `NBSANDBOX_OIDC_ID_PREFIX`, `NBSANDBOX_OIDC_ID_CLAIMS` (comma-separated). Setting the issuer turns tokens on. The issuer and key URLs must be `https` (plain `http` only on loopback, for a local provider). Startup refuses settings with problems and lists them all.

`id_prefix` and `id_claims` decide who owns which space. Choose them once: changing them later orphans every existing space, until an administrator gives each space to its owner's new ID (below).

### Revoked tokens

A token is normally good until it expires. With introspection (RFC 7662), nbsandboxd also asks the provider whether the token is still active, so a token revoked there (a user signed out, an account disabled, a session ended by an administrator) stops working within the cache time:

```yaml
auth:
  oidc:
    # ...
    introspection:
      client_id: nbsandbox      # a confidential client allowed to introspect
      client_secret: ""         # better: NBSANDBOX_OIDC_INTROSPECTION_CLIENT_SECRET
      # url: ""                 # default: introspection_endpoint from the discovery document
      # cache: 30s              # how long an "active" answer is trusted; at most 5m
```

- Only tokens that pass every local check are sent to the provider: a forged or expired token never leaves nbsandboxd.
- An "active" answer is cached for `cache`; an "inactive" one until the token would have expired, so a revoked token costs one call.
- **It fails closed:** if the provider can't be reached or refuses nbsandboxd's client, tokens are refused (`401`, logged as `introspection failed`). The provider's availability becomes nbsandbox's.
- Environment variables: `NBSANDBOX_OIDC_INTROSPECTION_CLIENT_ID`, `NBSANDBOX_OIDC_INTROSPECTION_CLIENT_SECRET`, `NBSANDBOX_OIDC_INTROSPECTION_CACHE`.
- **Keycloak:** give nbsandbox's client *Client authentication* on, no flows, and use its secret (tested by `make test-keycloak`: a token stops working right after the user signs out). **Entra ID** has no introspection endpoint; keep its tokens short-lived instead.

### Failed sign-ins

Password guessing against basic auth is slowed down. Each username may fail 5 times, then gets `429 Too Many Requests` with `Retry-After` (gRPC: `RESOURCE_EXHAUSTED`, with `retry-after` in the metadata) until the allowance refills, one failure per minute. While limited, the password isn't checked at all. A successful sign-in restores the username's whole allowance.

```yaml
auth:
  login_limit:
    failures: 5        # NBSANDBOX_LOGIN_LIMIT_FAILURES; 0 turns the limit off
    refill: 1m
    # per_address: true   # default: on when trusted_proxies is set
  trusted_proxies: [caddy]  # NBSANDBOX_TRUSTED_PROXIES (comma-separated)
```

**Per client address** too, against one address trying many usernames: but only when nbsandboxd knows the real address. Behind a proxy every request comes from the proxy, and limiting it would lock everyone out at once, so the address limit is on only when `trusted_proxies` names the proxies (addresses, CIDRs, or host names such as a Compose service, looked up every 30 s). nbsandboxd then takes the client from `X-Forwarded-For`: the nearest address that isn't a trusted proxy; anything a client writes further back is ignored. The Compose stack sets `caddy`; with Kubernetes, set your ingress controller's Pod CIDR. Set `per_address: true` only when clients reach nbsandboxd directly.

Bearer tokens aren't limited: a guessed token would have to be signed by the provider.

### Quotas per owner

Without user records, limits are set per caller ID in `settings.yaml`:

```yaml
uploads:
  max_space: 500MiB          # every space's quota
quotas:
  max_spaces: 20             # spaces per owner (0: no limit)
  owners:                    # exceptions, by caller ID
    - owner: entra:<tid>:<oid>
      max_spaces: 100
      max_space: 5GiB
```

Creating a space past the cap is `413` with code `payload_too_large` and a message naming the cap. The cap holds even when one owner creates many spaces at once.

### Administration

An operator holding `nbsandbox.admin` (an app role on a service account, or a basic user with `admin: true`) can:

| | |
|---|---|
| `GET /v1/admin/spaces?owner=` | every owner's spaces, with the owner, newest first |
| `PATCH /v1/admin/spaces/{id}` `{"owner": "..."}` | give a space to another caller ID: someone whose ID changed (an issuer or tenant move), or whose work a colleague takes over |
| `DELETE /v1/admin/spaces/{id}` | delete a space of someone removed from the provider |
| `GET /v1/admin/runs?status=&owner=&space=` | every owner's runs, with the owner (metadata only: no results or outputs) |
| `POST /v1/admin/runs/{run_id}/cancel` `{"reason": "..."}` | stop any run; its owner sees who and why |
| `GET` / `PUT /v1/admin/drain` `{"draining": true, "reason": "..."}` | drain the workers before maintenance: no queued run starts, running ones finish, new ones queue ([operations.md](operations.md#upgrade)) |
| `GET /v1/admin/audit?actor=&space=&action=&since=&until=` | the audit log ([operations.md](operations.md#audit-log)) |

The same operations are in gRPC's `AdminService`. Every one of them, reads included, is in the audit log with the administrator's ID.

### The calling application

Runs record the token's client (`azp`, `appid` or `client_id`) as `client`, shown in `GET .../runs/{id}`: with the On-Behalf-Of flow, the space is the end user's and `client` says which of your backends started the run. Basic-auth runs have none.

### Entra ID

1. **Register nbsandbox** (App registrations → New registration). Note its *Application (client) ID* and your *Directory (tenant) ID*.
2. **Token version 2:** in the app's *Manifest*, set `"requestedAccessTokenVersion": 2` (older manifests: `accessTokenAcceptedVersion`). Then the issuer is `https://login.microsoftonline.com/<tenant id>/v2.0` and `aud` is the client ID. (Version 1 tokens have issuer `https://sts.windows.net/<tenant id>/` and `aud` = the Application ID URI; that works too, if `issuer` and `audience` match.)
3. **Expose an API:** set the Application ID URI (`api://<client id>`) and add the delegated scopes from the table above (`nbsandbox.read`, ...). They arrive in `scp`.
4. **App roles** (optional): for an app calling as itself (client credentials, no user), add app roles with the same names (allowed member type: Applications) and grant them to that app. They arrive in `roles`; the caller ID is then the app's own `oid`.
5. **Your backend's registration:** add API permissions for nbsandbox's scopes, grant admin consent, and use the On-Behalf-Of flow above.
6. **Settings:** `issuer: https://login.microsoftonline.com/<tenant id>/v2.0`, `audience: <client id>`, `id_prefix: entra`, `id_claims: [tid, oid]`.

### Keycloak

Tested end to end with Keycloak 26.8 (`make test-keycloak`): a user's token runs a notebook, scopes and audience are enforced, and a backend calls on behalf of its user with token exchange. The test realm, [deploy/keycloak/realm.json](../deploy/keycloak/realm.json), is a complete example of the setup below (its passwords and secrets are public test values).

1. **A client for nbsandbox** in your realm, with no flows enabled: it only names the audience. Its client ID is nbsandbox's audience.
2. **Client scopes** named `nbsandbox.read`, `nbsandbox.runs`, ... (*Include in token scope* on), assigned as optional or default scopes to the client apps that call nbsandbox. They arrive in `scope`.
3. **Audience:** Keycloak's access tokens don't name nbsandbox by default. Give each nbsandbox client scope an *Audience* mapper (included client audience: nbsandbox), so tokens carrying them get `aud` = nbsandbox.
4. **The subject:** keep the realm's `basic` client scope (it puts `sub` in access tokens) as a default scope of every client.
5. **On behalf of users** (Keycloak's standard token exchange, on by default since 26.2):
   - your backend is a confidential client with *Standard token exchange* enabled (`standard.token.exchange.enabled`);
   - the user's token must be issued for the backend (its `aud` includes the backend: an audience mapper on a scope the user's app requests);
   - the nbsandbox scopes are optional scopes of the backend's client, so it may ask for them;
   - the backend posts `grant_type=urn:ietf:params:oauth:grant-type:token-exchange`, `subject_token=<the user's token>`, `subject_token_type=urn:ietf:params:oauth:token-type:access_token`, `audience=nbsandbox` and the scopes it needs, with its client credentials. The new token's `sub` is the user's, and runs record the backend as `client`.
6. **A service calling as itself:** a confidential client with service accounts enabled and the nbsandbox scopes it needs as default scopes (client credentials grant). The caller ID is the service account's `sub`.
7. **Settings:** `issuer: https://<host>/realms/<realm>`, `audience: <nbsandbox client id>`, `id_prefix: kc` (or your realm's name), `id_claims: [sub]`.

## Signing in to the web UI

The web UI can be served to your users: they sign in with the same identity provider, and in the UI they are the **same caller** as through the API (same caller ID, so the same spaces; same scopes).

```
browser ──► /ui/login ──► provider's sign-in page (authorization code + PKCE)
browser ◄── /ui/callback: nbsandboxd exchanges the code with the UI client's secret,
            checks the access token like any API token, keeps the tokens server-side (encrypted)
browser ◄── a session cookie (a random ID; HttpOnly, SameSite=Lax). No token reaches the browser.
```

```yaml
ui:
  enabled: true
  listen: 127.0.0.1:8090       # behind your proxy (Caddy in the Compose stack)
  auth: oidc                   # or basic: a sign-in form against users.yaml; none: development only
  public_url: https://nbsandbox.example.com
  session:
    idle: 1h                   # signed out after this long without a request
    max: 12h                   # and after this long in any case
  oidc:
    client_id: nbsandbox-ui
    client_secret: ""          # better: NBSANDBOX_UI_OIDC_CLIENT_SECRET
    scopes: [openid, profile, nbsandbox.read, nbsandbox.spaces.write, nbsandbox.files.write,
             nbsandbox.runs, nbsandbox.functions.invoke, nbsandbox.functions.publish]
```

- **The access token decides who the user is.** It is checked by the same rules as an API token (issuer, audience, caller ID, scopes, and introspection when it is on). A sign-in without `nbsandbox.read` is refused; other missing scopes give `403` on what they cover.
- **Sessions:** kept in PostgreSQL (any API replica serves them) and ended after `idle` or `max`. The access token is refreshed before it expires; when the provider refuses (the user signed out there, or was disabled), the session ends. With introspection on, a revoked token ends the session within its cache time.
- **Sign out** ends the session and then the provider's own session (RP-initiated logout), if the provider has an `end_session_endpoint`.
- **Changes need the UI's own pages:** requests that change something must carry the UI's header and, if they say, the UI's origin. A page on another site can do neither, so the cookie can't be used against the user (CSRF). `/v1` never accepts the cookie: it takes only bearer tokens and basic auth.
- **The client secret also encrypts the stored tokens:** rotating it signs everyone out.
- `public_url` must be `https` (plain `http` only on loopback, for a local provider) and is the only origin the UI accepts changes from: open the UI at that address.
- **`auth: basic`** signs in with a username and password from `users.yaml`, with the same [failed sign-in limit](#failed-sign-ins) as the API. **`auth: none`** is the development mode: pick any user, no password. It listens on loopback only (`no_login_beyond_loopback` is for the dev Compose stack, whose container port is published on the host's loopback).
- Environment variables: `NBSANDBOX_UI_AUTH`, `NBSANDBOX_UI_PUBLIC_URL`, `NBSANDBOX_UI_SESSION_IDLE`, `NBSANDBOX_UI_SESSION_MAX`, `NBSANDBOX_UI_OIDC_CLIENT_ID`, `NBSANDBOX_UI_OIDC_CLIENT_SECRET`, `NBSANDBOX_UI_OIDC_SCOPES`.

**Serving it.** Put the UI behind the same host name as the API: `/v1/` and `/nbsandbox.v1.*` to the API port, everything else to the UI port. The Compose stack does this when `NBSANDBOX_UI_ENABLED=true` (with `NBSANDBOX_UI_OIDC_CLIENT_ID` and `NBSANDBOX_UI_OIDC_CLIENT_SECRET` in `.env`); the Helm chart with `ui.enabled` and the Ingress ([kubernetes.md](kubernetes.md#install-with-helm)).

**Keycloak** (tested by `make test-keycloak`: sign-in through the real login page, a run as the same caller as a bearer token, sign-out, and a revoked session refused):
1. A client `nbsandbox-ui`: *Client authentication* on, *Standard flow* only, *Valid redirect URIs* `<public_url>/ui/callback`, *Valid post logout redirect URIs* `<public_url>/ui/signed-out`, and PKCE method `S256` (Advanced).
2. The nbsandbox client scopes (with their audience mappers, above) and `profile` as optional scopes of that client.
3. Keep `offline_access` out of the scopes: an offline token outlives the user's Keycloak session, so signing out there wouldn't end the UI session.

**Entra ID:**
1. A new app registration (*Web* platform) with the redirect URIs `<public_url>/ui/callback` and `<public_url>/ui/signed-out` (Entra ID checks the address it returns to after sign-out against them), and a client secret.
2. API permissions: nbsandbox's delegated scopes; grant admin consent.
3. Scopes in `ui.oidc.scopes`: `openid`, `profile`, `offline_access` (Entra ID gives refresh tokens only with it) and nbsandbox's as `api://<nbsandbox client id>/nbsandbox.read`, ... A token request to Entra ID names one API, which is all the UI needs.

## Troubleshooting

nbsandboxd logs each refused token with the reason (`bearer token refused`); the caller sees only `invalid_token`.

| Log reason | Likely cause |
|---|---|
| `issuer "..." is not "..."` | Token version or tenant differs from `issuer` (Entra: v1 vs v2) |
| `audience is not "..."` | A token issued for another API, e.g. your backend's own token: exchange it first |
| `expired at ...` | Clock skew over 60 s, or a cached token used too long |
| `unknown signing key` | The key set can't be fetched, or the token comes from another provider |
| `claim oid is missing` | `id_claims` names a claim this token type doesn't carry |
| `valid for ..., longer than ...` | Raise `max_lifetime`, or shorten token lifetime at the provider |
| `revoked (introspection says inactive)` | Revoked at the provider (signed out, disabled): expected |
| `introspection failed` | The provider is unreachable, or rejects nbsandboxd's client ID or secret |

`403 forbidden` names the missing scope in `WWW-Authenticate`: add it to the token's scopes or app roles.

## Not yet

- Sharing a space with another caller (grants keyed by caller ID).
