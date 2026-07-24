# Use the OpenList token broker for 123Pan authentication

## Context

- 123Pan does not expose third-party application credentials to individual developers.
- OpenList provides an online token broker compatible with its `123cloud_oa` driver.
- The broker receives a 123Pan refresh token and returns a new access token,
  refresh token, and expiry.

## Decision

- Accept a refresh token as the backend's authentication credential.
- Use `https://api.oplist.org/123cloud/renewapi` as the default token broker.
- Provide an advanced `token_server` option for a compatible replacement.
- Disclose during configuration that the refresh token is sent to the selected
  token server.
- Use the OpenList-compatible GET request with the refresh token in the
  `refresh_ui` query parameter.
- Do not add special query-parameter redaction to `fshttp` for this backend.
- Persist every rotated refresh token before relying on it for a later refresh.

## Consequences

- The selected token broker is part of the backend's authentication trust boundary.
- Losing a newly rotated refresh token can require the user to authenticate again.
- A compatible self-hosted token broker can replace the OpenList service
  without changing the filesystem API implementation.
- HTTP request dumps, curl dumps, proxies, and token-server access logs may
  contain the refresh token, so those logs are part of the authentication trust
  boundary.
