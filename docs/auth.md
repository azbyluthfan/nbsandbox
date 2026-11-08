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
```

The provider's keys are fetched from its discovery document (or `jwks_url`), cached, refreshed hourly, and refetched when a token names an unknown key (at most once a minute). If the provider is unreachable, keys already loaded keep working. Tokens are never logged; refused tokens are logged with the reason, and the caller only gets `401` with `WWW-Authenticate: Bearer error="invalid_token"`.

**Spaces belong to the caller ID.** There is no user table: a caller who presents a valid token can create spaces and use their own. Removing someone at the provider ends their access as soon as their tokens expire, so keep access tokens short-lived (5–15 minutes). Basic-auth users are their username (`alice`); a token caller is always `<prefix>:...`, so the two can never collide.

## Scopes

Each endpoint needs one scope (listed per operation in [api/openapi.yaml](../api/openapi.yaml)). Without it: `403`, code `forbidden`, and `WWW-Authenticate: Bearer error="insufficient_scope", scope="..."`. Basic-auth users hold every scope except `nbsandbox.admin`, which a basic user gets only with `admin: true` in users.yaml.

| Scope | Allows |
|---|---|
| `nbsandbox.read` | list and read spaces, files, runs, results, logs and streams; the runtime |
| `nbsandbox.spaces.write` | create, rename and delete spaces |
| `nbsandbox.files.write` | upload, edit, move and delete files and folders; promote artifacts |
| `nbsandbox.runs` | start, cancel and delete runs |
| `nbsandbox.functions.invoke` | call functions |
| `nbsandbox.functions.publish` | publish and delete functions |
| `nbsandbox.admin` | every owner's spaces: list them, give one to another owner, delete one (`/v1/admin/spaces`, gRPC `AdminService`). Grant it only to operators: it is never implied by the others |

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

Each change is logged with the administrator's ID.

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

1. **A client for nbsandbox** in your realm (bearer-only / no flows enabled). Its client ID is nbsandbox's audience.
2. **Client scopes** named `nbsandbox.read`, `nbsandbox.runs`, ... assigned (optional or default) to the client apps that call nbsandbox. They arrive in `scope`.
3. **Audience:** Keycloak's access tokens don't name nbsandbox by default. Add an *Audience* mapper (included client audience: nbsandbox) to the nbsandbox client scopes, so tokens carrying them get `aud` = nbsandbox.
4. **On behalf of users:** enable token exchange for your backend's client and exchange users' tokens with `audience=<nbsandbox client id>`.
5. **Settings:** `issuer: https://<host>/realms/<realm>`, `audience: <nbsandbox client id>`, `id_prefix: kc` (or your realm's name), `id_claims: [sub]`.

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

`403 forbidden` names the missing scope in `WWW-Authenticate`: add it to the token's scopes or app roles.

## Not yet

- Sharing a space with another caller (grants keyed by caller ID).
- Token introspection (RFC 7662), for revocation before expiry.
- Signing in to the web UI with the provider (it stays a local, development-only tool).
