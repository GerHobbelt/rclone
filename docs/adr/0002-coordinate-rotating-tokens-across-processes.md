# Coordinate rotating tokens across processes

## Context

- A 123Pan refresh exchanges both the access token and the refresh token.
- The refresh token is one-time: after a successful exchange, independently
  retrying the old refresh token is unsafe.
- Multiple rclone processes can use the same config section concurrently.
- The required coordination boundary is multiple rclone processes on the same
  machine sharing one local persistent config.
- Rclone may derive runtime names such as `abc{A1fie}:` when a remote is created
  with overridden options.
- The derived suffix is part of runtime filesystem identity, not persistent
  config identity.
- A process-local mutex cannot prevent two processes from consuming the same
  refresh token.
- Atomic config-file replacement prevents partial files but does not make a
  read-exchange-write sequence atomic.
- Independent processes updating different sections can still overwrite each
  other's whole-file config saves without a shared commit lock.

## Decision

- Introduce a reusable transactional rotating-token source outside individual
  backends.
- Store the OAuth credential and versioned transaction metadata together in the
  existing single `token` JSON value.
- Preserve the standard OAuth JSON fields and add a nested
  `rclone_token_state` object.
- Make the transactional token source the sole writer of the extended value.
- Pass the persistent config section identity to it structurally through the
  config mapper or token store.
- Never derive the config section by removing a `{...}` suffix from a runtime
  name.
- Serialize refreshes with:
  - A process-local mutex.
  - A per-section cross-process token lock tied to a stable sidecar path.
  - A per-file cross-process config commit lock tied to another stable sidecar
    path.
- Hold the token lock throughout the broker exchange.
- Hold the config commit lock only across strong reload, mutation, and durable
  atomic replacement.
- Make every config writer participate in the config commit lock.
- Always acquire the token lock before the config commit lock when both are
  needed.
- Under those locks:
  - Strongly reload the durable token state.
  - Adopt an already-refreshed generation when one exists.
  - Persist and flush an `in_flight` claim before sending the refresh token.
  - Release the config commit lock during the broker exchange.
  - Reacquire it and verify the claimed owner and generation.
  - Persist and flush the new token pair as the next `ready` generation before
    using it.
- Tag authorization failures with the generation that produced them and
  invalidate only when that generation remains current.
- Refresh on demand before authenticated requests when the token enters a
  dynamic lead window capped at 10 minutes.
- Do not run refresh work merely because a configured remote is idle.
- Allow active long-running operations to schedule renewal through the same
  transactional token source.
- Retry an API request at most once after an explicit access-token-invalid
  response.
- Do not route authorization failures through an unbounded general pacer retry.
- Treat `in_flight` observed after acquiring the lock as an orphaned,
  indeterminate exchange.
- Fail closed for an orphaned exchange:
  - Mark it `uncertain`.
  - Do not retry the claimed refresh token.
  - Require reauthentication or authoritative broker-side recovery.
- Restore the original `ready` generation only when the transport proves that
  the exchange request was not sent.
- Mark an exchange `uncertain` whenever the request may have reached the broker
  but no complete, validated token response is available.
- Mark a definitively invalid, expired, revoked, or rejected refresh credential
  as `reauth_required`.
- Do not retry token exchange through the general request pacer.
- Make `rclone config reconnect remote:` the only client operation that may
  replace an `uncertain` or `reauth_required` state with a new `ready`
  generation.
- Run reconnect under the same token lock and leave the terminal state
  unchanged after cancellation, failure, or another indeterminate exchange.
- Do not let a normal config update clear the state-machine metadata.
- Require a shared, writable, durable token store for automatic rotation.
- Reject environment-only, connection-string-only, and other non-persistent
  rotating credentials.
- Reject rotating credentials supplied through higher-priority ephemeral
  overrides, even when a setter could write a replacement to the config file.
- Expose credential provenance through the config/token-store abstraction
  instead of detecting individual environment variables in each backend.
- Limit the guarantee to one machine and a local filesystem with reliable
  locking, atomic replacement, and durability semantics.
- Do not treat the sidecar lock as a distributed lock for NFS, SMB, unrelated
  containers, or multiple machines.
- Require every concurrently running process that uses the config to
  participate in the transactional lock protocol.
- Exclude mixed old-and-new rclone processes from the compatibility guarantee
  and test matrix.
- Lazily migrate a legacy 115 token under the shared lock:
  - Strongly reload it from persistent config.
  - Treat it as `ready` generation 1.
  - Persist the missing transaction metadata.
  - Do not call the token broker solely for migration.

## Consequences

- Process B reads process A's new access and refresh tokens by waiting for the
  token lock and then strongly reloading the next persisted generation.
- An active `in_flight` state is not a second refresh opportunity: another
  process cannot acquire the lock while the owner is active.
- A process that sees `in_flight` after acquiring the lock knows the owner is
  gone, but cannot know whether the broker consumed the refresh token.
- Holding a file lock during a network request increases lock duration, so
  waiters for the same token need bounded waits and actionable errors.
- Different config sections can exchange concurrently without allowing their
  whole-file config replacements to overwrite each other.
- The config subsystem gains a cross-process commit transaction used by
  ordinary config writers as well as token writers.
- Cross-machine coordination requires a broker-side atomic protocol or a
  separate shared coordination service and remains out of scope.
- An older rclone process can refresh without taking the new lock, so no new
  implementation can make mixed-version concurrent refresh safe unilaterally.
- Request-driven refresh avoids rotating a one-time credential for an idle
  remote.
- A single conditional authorization retry recovers from expiry without
  allowing an invalid-token response to create a refresh loop.
- A client-side implementation cannot automatically recover when the broker
  rotates a token but its response is lost.
- Conservative failure classification can require reauthentication after a
  broker or proxy error that did not actually consume the refresh token.
- Full automatic recovery would require an idempotency or recovery contract from
  the token broker.
- Recovery is explicit and auditable, but it requires user action and a fresh
  provider credential.
- The reusable token source can replace 115's runtime-name suffix trimming and
  process-local-only refresh coordination.
- Existing valid 115 credentials migrate without reauthentication or an
  unnecessary token exchange.
- A single config-value replacement cannot expose a generation whose access
  token, refresh token, and state metadata came from different writes.
- Standard OAuth decoders ignore the nested state extension and can still read
  the credential fields.
- Ephemeral overrides remain available for non-rotating options but cannot
  reintroduce an already-consumed refresh token on the next process start.
- Backends remain responsible only for their provider-specific exchange
  request and response mapping.

## Alternatives rejected

- **Strip `{...}` from the runtime name**:
  - This depends on an incidental naming format.
  - It can confuse legitimate config names containing braces.
  - It does not solve cross-process refresh ordering.
- **Use only a process-local mutex**:
  - Separate processes can still exchange the same refresh token.
- **Release the lock during the broker call**:
  - Other processes could observe the claim but cannot safely determine whether
    to wait, retry, or take over after a crash.
- **Use only a per-section token lock**:
  - Different sections can still race while replacing the same whole config
    file.
- **Hold one config-file lock throughout every broker call**:
  - It is safe but a slow broker for one remote blocks unrelated remotes from
    refreshing or committing config.
- **Let every process maintain its own token pair**:
  - One process can invalidate another process's refresh token and cause a
    refresh storm.
- **Retry the old refresh token after timeout**:
  - A timeout does not prove that the broker failed before consuming the token.
- **Clear orphaned `in_flight` state after a timeout**:
  - Elapsed time does not resolve the uncertain broker outcome.
- **Rotate a token read from an ephemeral override and write the result to the
  config file**:
  - The override remains higher priority on the next process start.
  - It can reintroduce the consumed refresh token.
  - Copying the replacement to disk also violates the expected lifetime of an
    ephemeral secret.
- **Store token state in several independently saved config keys**:
  - A crash between writes can expose mismatched credentials, generation, and
    refresh status.
- **Continue writing through `oauthutil.PutToken`**:
  - Marshaling only `oauth2.Token` discards the transaction metadata and turns
    the next load into a false legacy migration.
