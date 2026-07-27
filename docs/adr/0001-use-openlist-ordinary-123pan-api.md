# Use OpenList's ordinary 123Pan API protocol

## Context

- OpenList's `123_open` driver and its online refresh-token API are not a
  reliable basis for this backend.
- OpenList also contains an ordinary `drivers/123` implementation that signs
  web API requests and establishes a session through account login.
- The ordinary protocol supports the required filesystem operations, including
  server-side move.
- A captured current web request confirms server-side copy is an asynchronous
  ordinary API task.

## Decision

- Use OpenList's ordinary `123` driver as the protocol source.
- Authenticate with a configured account username and obscured password.
- Persist the returned session token as rclone's sensitive `token` option.
- Use the ordinary signed `/b/api` request flow.
- Re-login and retry a failed ordinary API request once after an explicit 401.
- Implement server-side copy with the ordinary asynchronous copy task API.
- Do not configure an OpenList token broker, a refresh token, or cross-process
  refresh coordination for 123Pan.

## Consequences

- The backend no longer depends on an external token-broker service.
- A later rclone process can reuse the persisted web session token and avoids a
  login while the provider still accepts it.
- A 401 replaces the persisted session token by signing in with the stable
  configured account credentials.
- A persistent rclone configuration contains normal account options and a
  sensitive access token, but no one-time refresh token.
- Ordinary web endpoints can change without a public Open Platform stability
  guarantee, so the protocol tests explicitly check each supported request
  shape.
