---
title: Authentication
menuTitle: Authentication
description: Authenticate the Grafana MCP server to Grafana using browser-based SSO or a service account token.
keywords:
  - authentication
  - service account
  - token
  - MCP
weight: 1
aliases:
  - /docs/grafana-cloud/machine-learning/mcp/configure/authentication/
---

# Authentication

The Grafana MCP server needs credentials to call the Grafana API. When `GRAFANA_SERVICE_ACCOUNT_USERNAME` and `GRAFANA_SERVICE_ACCOUNT_PASSWORD` are set, browser-based SSO is the default authentication method. Otherwise, the server uses a service account token when one is configured.

## What you'll achieve

You set the environment variables (or equivalent) so the server can authenticate to your Grafana instance. The same credentials apply whether you run the server with uvx, Docker, a binary, or Helm.

## Before you begin

- A Grafana instance (Grafana 9.0 or later).
- For token auth: a [service account](https://grafana.com/docs/grafana/latest/administration/service-accounts/#add-a-token-to-a-service-account-in-grafana) with a token and the RBAC permissions needed for the tools you use.

## Use browser-based SSO

Set `GRAFANA_URL`, `GRAFANA_SERVICE_ACCOUNT_USERNAME`, and `GRAFANA_SERVICE_ACCOUNT_PASSWORD`. The server creates a shared SSO session manager at startup and opens headless Chromium when the first Grafana API request arrives. It stores the returned Grafana session cookies and reuses them for later tool requests. `Set-Cookie` updates, including session rotation, are applied to the shared session. If Grafana rejects an expired session, the server logs in again once and retries the request when its body is replayable.

The first Grafana API request triggers a GET to Grafana's `/login/azuread` route. The authorize URL, OAuth `state`, PKCE challenge, tenant ID, client ID, and redirect URI are generated or provided by Grafana; do not copy them into environment variables. Optional settings:

- `GRAFANA_AUTH_MODE`: `sso` or `token`. Defaults to SSO when the SSO username/password are present, otherwise token mode. Set to `token` to override SSO when both credential types are available.
- `GRAFANA_LOGIN_PATH`: Grafana SSO route; defaults to `/login/azuread`.
- `GRAFANA_SSO_HOSTS`: comma-separated exact HTTPS identity-provider hosts allowed to receive credentials; defaults to `login.microsoftonline.com`.
- `GRAFANA_SSO_TIMEOUT`: login timeout; defaults to `120s`.
- `GRAFANA_SSO_CHROME_BIN` or `CHROME_BIN`: Chromium executable path.
- `GRAFANA_SSO_LOG_FILE`: append structured SSO stage logs to this file (mode `0600`). Logs omit credentials and OAuth query parameters.
- `GRAFANA_SSO_SCREENSHOT_DIR`: save redacted PNGs at major login stages. The directory is created with mode `0700`; each image is written with mode `0600`.

The headless login cannot complete MFA or other interactive identity-provider challenges. It fails instead of attempting to bypass them. In containers Rod runs Chromium with its container-specific no-sandbox option; the MCP image runs as a non-root user.

## Use a service account token

Set `GRAFANA_URL` and `GRAFANA_SERVICE_ACCOUNT_TOKEN` in the environment passed to the server.

- **GRAFANA_URL:** Your Grafana base URL (for example, `http://localhost:3000` or `https://myinstance.grafana.net`).
- **GRAFANA_SERVICE_ACCOUNT_TOKEN:** The token you created for the service account.

The deprecated `GRAFANA_API_KEY` is still supported but will be removed in a future version; use `GRAFANA_SERVICE_ACCOUNT_TOKEN` instead.

### Read the token from a file

Set `GRAFANA_SERVICE_ACCOUNT_TOKEN_FILE` to a file path that contains the token instead of passing the value inline. The file is read fresh on every request, so a rotated token is picked up automatically without restarting the server.

This is useful in Kubernetes, where a Secret mounted as a volume is updated in place when the underlying Secret changes. Because the server's client cache is keyed on the token value, a rotated token transparently produces a new client with no pod restart:

```yaml
env:
  - name: GRAFANA_SERVICE_ACCOUNT_TOKEN_FILE
    value: /var/run/secrets/grafana/token
volumeMounts:
  - name: grafana-token
    mountPath: /var/run/secrets/grafana
    readOnly: true
volumes:
  - name: grafana-token
    secret:
      secretName: grafana-mcp-token
```

Surrounding whitespace (including a trailing newline) is trimmed from the file contents. If both `GRAFANA_SERVICE_ACCOUNT_TOKEN` and `GRAFANA_SERVICE_ACCOUNT_TOKEN_FILE` are set, the inline token takes precedence.

For HTTP deployments that [select a Grafana URL per request](../command-line-flags/#select-a-grafana-url-per-request), each request must supply a token for its selected instance. The server does not reuse `GRAFANA_SERVICE_ACCOUNT_TOKEN` or the token file for that request.

## Use username and password

You can use basic auth by setting `GRAFANA_USERNAME` and `GRAFANA_PASSWORD` with `GRAFANA_AUTH_MODE=token`. This is less suitable for automation; prefer SSO or a service account token.

## Next steps

- [Enable and disable tools](../enable-and-disable-tools/) to control which MCP tools are available.
- [Transports and addresses](../transports-and-addresses/) for stdio, SSE, and streamable-http.
