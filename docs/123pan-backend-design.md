# 123Pan backend design

## Status

- Accepted and implemented.
- The implementation is covered by focused unit tests and the standard rclone
  backend integration test can be enabled with a configured `Test123Pan:`
  remote.

## Authentication

- Accept a 123Pan refresh token from the user.
- Exchange and rotate the token through an OpenList-compatible token broker.
- Default to `https://api.oplist.org/123cloud/renewapi`.
- Allow an advanced `token_server` override.
- Follow the OpenList-compatible GET contract, including the refresh token in
  the `refresh_ui` query parameter.
- Do not add backend-specific or `fshttp` query-parameter redaction.
- Document that HTTP request and curl dumps, proxies, and server access logs may
  contain the refresh token.
- See [ADR 0001](adr/0001-use-openlist-token-broker-for-123pan.md).
- Coordinate rotating credentials across processes through a shared transactional
  token source.
- Keep the persistent config section identity separate from rclone's derived
  runtime instance name.
- See [ADR 0002](adr/0002-coordinate-rotating-tokens-across-processes.md).

## Token rotation

### Identities

- The config section identity is the section that owns the credential, such as
  `[abc]`.
- The runtime instance name may be derived by rclone, such as
  `abc{A1fie}:`, when options override the configured remote.
- The config mapper must carry the config section identity into the token store.
- Authentication code must not parse, trim, or otherwise reconstruct a config
  section identity from the runtime instance name.
- Derived runtime names remain available for logging, cache keys, and filesystem
  identity.

### Credential provenance

- A rotating refresh token must originate from the writable local persistent
  config section.
- Do not accept a rotating token supplied by:
  - A connection string.
  - A command-line or backend flag override.
  - A remote-specific environment variable.
  - A backend-wide environment variable.
- Continue to allow those sources for ordinary non-rotating options.
- Reject an overridden rotating token during `NewFs` with an actionable error
  that tells the user to store it in the local config.
- Do not silently:
  - Copy an overridden secret into the config file.
  - Ignore rclone's normal option precedence.
  - Fall back to process-local refresh coordination.
- Give the shared token module a formal way to distinguish effective option
  values from persistent config values.
- Do not make each backend discover credential provenance by parsing environment
  variable names or connection strings.

### Shared token source

- Put the refresh protocol in a reusable module rather than in the 123Pan
  backend.
- The module owns:
  - In-process serialization.
  - Cross-process serialization.
  - Strong reloads from persistent config.
  - Token generations.
  - Compare-and-invalidate behavior.
  - Atomic persistence.
  - Refresh failure classification.
- The 123Pan backend supplies only the broker exchange operation.
- The same module can replace 115's suffix trimming and process-local refresh
  coordination.

### Legacy 115 migration

- Migrate an existing 115 `token` lazily on its first use by the transactional
  token source.
- Acquire the sidecar lock and strongly reload the legacy token before
  migrating it.
- Treat a valid legacy token as `ready` generation 1.
- Persist only the missing transaction metadata during migration.
- Do not contact the token broker merely to perform the migration.
- Do not require the user to authenticate again merely to perform the
  migration.
- If another process completes the migration first, reload and adopt its
  generation 1 state.
- Refresh normally only when the migrated token is expired or rejected by the
  service.

### Persistent state

- Persist the complete token and refresh state in the existing single `token`
  config value.
- Keep the standard OAuth JSON fields:
  - `access_token`.
  - `token_type`.
  - `refresh_token`.
  - `expiry`.
- Add a nested `rclone_token_state` object containing:
  - A state format `version`.
  - The token `generation`.
  - The refresh `status`.
  - The refresh `owner`.
  - The `refresh_token_hash`.
- Start the state format at version 1.
- Commit the standard token fields and `rclone_token_state` with one config
  value replacement.
- Use these refresh states:
  - `ready`: the stored token generation may be used.
  - `in_flight`: a process has durably claimed the stored refresh token and may
    already have consumed it at the broker.
  - `uncertain`: automatic refresh is unsafe because the outcome of a previous
    exchange cannot be proved.
  - `reauth_required`: the provider or broker definitively rejected or revoked
    the refresh credential.
- Record a refresh owner identifier and a hash of the claimed refresh token for
  diagnostics.
- Never log or persist an additional plaintext copy of the refresh token.
- Make the transactional token source the only writer of the extended `token`
  value.
- Do not pass the extended value through an old writer such as
  `oauthutil.PutToken`, because remarshal through `oauth2.Token` would discard
  `rclone_token_state`.
- Continue to decode the standard OAuth fields so code using `oauth2.Token` can
  read a `ready` credential while ignoring the nested extension.
- Treat a legacy token without `rclone_token_state` as the migration case, not
  as corrupt state.

### Refresh transaction

- Use two cross-process locks:
  - A token lock identified by the persistent config file and config section.
  - A config commit lock identified by the persistent config file.
- Use stable sidecar lock paths rather than locking the config file inode,
  because config writes replace that inode atomically.
- Hold the token lock throughout the refresh, including the broker exchange.
- Hold the config commit lock only while strongly reloading, modifying, and
  durably replacing the config file.
- Make every config writer in the new implementation participate in the config
  commit lock, not only the 123Pan and 115 token writers.
- Always acquire locks in this order:
  - Token lock.
  - Config commit lock.
- Never acquire a token lock while already holding the config commit lock.
- After acquiring the token lock, acquire the config commit lock and strongly
  reload the token state.
- If another process has already stored a usable newer generation:
  - Adopt that generation.
  - Release both locks.
  - Do not call the token broker.
- Otherwise:
  - Persist `in_flight` with the current generation, owner, and refresh-token
    hash.
  - Flush that state durably.
  - Release the config commit lock.
  - Keep holding the token lock while calling the token broker.
  - Reacquire the config commit lock.
  - Strongly reload and verify the expected `in_flight` owner and generation.
  - Persist the returned access token and rotated refresh token as the next
    `ready` generation.
  - Flush the new generation durably.
  - Update the process-local token.
  - Release the config commit lock.
  - Release the token lock.
- Do not use a newly returned access token before its matching rotated refresh
  token has been persisted.
- A process waiting for either lock must use a bounded wait and must not bypass
  the lock by refreshing or saving independently.
- Different config sections may call their brokers concurrently, but commits to
  one config file remain serialized.

### Concurrent readers

- Initial token loading and every refresh decision must enter the locked
  transaction and perform a strong reload.
- While process A is actively refreshing, it owns the token lock.
- Process B therefore waits at the token lock and cannot observe A's active
  `in_flight` state as an actionable refresh opportunity.
- After A persists generation 8 and releases the lock, B reloads generation 8
  and uses its new access and refresh tokens.
- A process may continue using a generation already held in memory until it
  expires or the service rejects it.

### Late authorization failures

- Associate every in-memory access token with its persisted generation.
- On an authorization failure, invalidate only if the failed generation is
  still current.
- For example, a late failure for generation 7 must not invalidate generation 8
  already written by another process.
- After a compare-and-invalidate miss, reload and retry with the newer
  generation instead of refreshing again.

### Refresh triggers

- Do not refresh a token merely because a configured remote is idle.
- Before an authenticated API request:
  - Check the current token expiry.
  - Enter the shared refresh transaction when the token is inside its refresh
    window.
- Use a dynamic refresh lead with an upper bound of 10 minutes before expiry.
- Shorten the lead proportionally for tokens whose lifetime is too short for a
  10-minute lead.
- An active long-running operation may run a renewal timer.
- A renewal timer must call the same transactional token source and must not
  implement a separate refresh path.
- Treat only an explicit access-token-invalid response as an authorization
  refresh trigger.
- For that response:
  - Compare-and-invalidate the generation used by the failed request.
  - Reload or refresh through the shared transaction.
  - Retry the API request at most once.
- Return a second authorization failure without another refresh attempt.
- Do not let the general request pacer repeatedly retry an authorization
  failure.
- Do not refresh in response to:
  - A timeout.
  - An HTTP 5xx response.
  - A rate-limit response.
  - An ordinary provider business error.

### Interrupted refreshes

- If a process acquires the token lock and then reloads an `in_flight` state, the
  previous lock owner is no longer active.
- Treat that state as orphaned and transition it to `uncertain`.
- Do not:
  - Retry the stored one-time refresh token.
  - Clear the state based only on elapsed time.
  - Fall back to a process-local token.
- Require reauthentication or an authoritative recovery operation supplied by
  the token broker.
- This fail-closed behavior is necessary because a crashed client cannot know
  whether the broker consumed the refresh token before the response was lost.

### Exchange failure classification

- The token exchange path must not use the general request pacer.
- Classify a failure by whether the request could have reached the token broker.
- If the request is provably not sent:
  - Restore the claimed generation from `in_flight` to `ready`.
  - Permit a later bounded refresh attempt.
- Examples that can be classified as not sent when confirmed by the transport:
  - Request construction failure.
  - DNS resolution failure.
  - Connection establishment failure.
  - TLS handshake failure before the HTTP request is written.
- If the request may have reached the broker, transition to `uncertain`.
- Ambiguous outcomes include:
  - A timeout or context cancellation after transmission may have begun.
  - A connection reset or unexpected EOF.
  - An HTTP 429 or 5xx response.
  - An unparseable response.
  - A nominally successful response missing the access token, rotated refresh
    token, or expiry.
- If the broker definitively reports an invalid, expired, revoked, or rejected
  refresh credential, transition to `reauth_required`.
- Only a complete, validated success response may commit the next `ready`
  generation.
- Never retry an ambiguous or definitively rejected exchange automatically.

### Explicit recovery

- Use `rclone config reconnect remote:` as the only operation that may recover
  an `uncertain` or `reauth_required` token state.
- Ordinary filesystem operations in either terminal state must:
  - Avoid the token broker.
  - Return an actionable error naming the reconnect command.
- The reconnect flow must acquire the same token lock used by runtime
  refreshes.
- Under that lock:
  - 123Pan asks for a fresh refresh token and exchanges it through the
    configured token broker.
  - 115 performs device authorization again.
- On successful reauthentication:
  - Persist the new access and refresh tokens as a new `ready` generation.
  - Clear the terminal state, refresh owner, and old refresh-token hash.
  - Flush the new state durably before releasing the lock.
- If the user cancels or reauthentication fails, leave the existing terminal
  state unchanged.
- A normal `rclone config update ... token=...` must not overwrite managed
  token metadata or bypass the reconnect protocol.
- If reconnect itself becomes indeterminate after sending a one-time credential,
  leave the state `uncertain`.

### Storage requirements

- The cross-process coordination guarantee covers rclone processes on the same
  machine that share the same local persistent config.
- Every process concurrently using that config must use the transactional token
  implementation.
- Concurrent use by older rclone versions that do not participate in the shared
  lock protocol is outside the design and test scope.
- The safety guarantee requires shared, writable, durable configuration.
- Reject automatic rotation when the credential originates only from an
  environment variable, a connection string, or another non-persistent mapper.
- Atomic config replacement alone is insufficient; the read, refresh claim,
  exchange, and final write belong to one cross-process transaction.
- Do not claim distributed coordination for:
  - Multiple machines.
  - Containers without a shared local lock namespace.
  - Config files stored on NFS, SMB, or another filesystem whose locking and
    durability semantics are not equivalent to a local filesystem.
- Distributed coordination requires a broker-side atomic protocol or another
  shared coordination service and is outside the initial design.

### Required tests

- A derived runtime name writes credentials only to its original config section.
- A legacy 115 token migrates to `ready` generation 1 without a broker request.
- A `token` write atomically contains matching OAuth fields and state metadata.
- A round trip through the transactional token codec preserves all version 1
  state.
- A standard OAuth decoder can read the OAuth fields and ignore
  `rclone_token_state`.
- No transactional write path calls a legacy token writer that strips the
  extension.
- Two subprocesses racing to migrate one legacy 115 token persist one
  generation 1 state without refreshing it.
- Two subprocesses sharing one config and one expiring generation result in
  exactly one broker exchange.
- Two different sections may perform broker exchanges concurrently while both
  final token values survive serialized config commits.
- Concurrent token and ordinary config writes preserve changes from both
  processes.
- Every lock path remains stable when an atomic config save replaces the config
  file inode.
- Every code path respects the token-lock-then-config-lock ordering.
- The waiting subprocess reloads and adopts the generation written by the
  refresher.
- A late authorization failure for generation 7 does not invalidate generation
  8.
- Two processes entering the refresh window result in one exchange and both use
  the new generation.
- An authorization failure retries once with a newer generation.
- A second authorization failure is returned without another exchange.
- Timeouts, 5xx responses, rate limits, and business errors do not invalidate a
  token.
- A long-running-operation renewer uses the shared transaction path.
- A subprocess killed after persisting `in_flight` leaves an orphan that fails
  closed.
- A broker timeout after `in_flight` transitions the token to `uncertain` and
  does not retry the claimed refresh token.
- A provably unsent exchange restores the original `ready` generation.
- A 429, 5xx, transport ambiguity, malformed response, or incomplete success
  transitions to `uncertain` without a retry.
- A definitive invalid-refresh-token response transitions to
  `reauth_required`.
- Filesystem operations in `uncertain` state do not call the broker and report
  the reconnect command.
- A successful reconnect writes a new generation and clears `uncertain`.
- A cancelled or failed reconnect preserves `uncertain`.
- A normal config update cannot overwrite a managed rotating token.
- A non-persistent credential source is rejected with an actionable error.
- A token override is rejected without writing the overridden secret to disk.
- Non-rotating option overrides continue to use normal rclone precedence.

## Glossary

- **Config section identity**:
  - The stable persistent owner of a remote's configuration, such as `[abc]`.
- **Runtime instance name**:
  - The possibly derived name rclone assigns to an `Fs`, such as
    `abc{A1fie}:`.
- **Token generation**:
  - A monotonically increasing identifier for one matched access-token and
    refresh-token pair.
- **Active refresh**:
  - An `in_flight` refresh whose owner still holds the token lock.
- **Orphaned refresh**:
  - An `in_flight` refresh observed after its owner has released or lost the
    token lock without committing a new generation.
- **Strong reload**:
  - A read from the durable shared config performed after acquiring the
    config commit lock, without trusting process-local cached config.
- **Compare-and-invalidate**:
  - Invalidating a rejected token only when its generation still matches the
    current persisted generation.
- **Reauthentication required**:
  - A terminal state proving that the refresh credential is unusable, rather
    than an indeterminate exchange outcome.
- **Coordination boundary**:
  - Processes on one machine sharing one local persistent config and its
    associated sidecar locks.
- **Token lock**:
  - A per-config-section lock held across the complete one-time refresh
    exchange.
- **Config commit lock**:
  - A per-config-file lock held only across strong reload, mutation, and durable
    atomic replacement.
- **Participating process**:
  - A process using the transactional token implementation and its shared lock
    protocol.
- **Credential provenance**:
  - The source from which a credential was read, including whether it came from
    durable config or a higher-priority ephemeral override.

## Initial scope

### Included

- List files and directories.
- Upload and download files.
- Create directories.
- Move deleted entries to the recycle bin.
- Rename and move entries.
- Copy files on the server.
- Report MD5 hashes.
- Report storage usage.

### Deferred

- Direct-link space.
- Share links.
- Offline downloads.
- Video transcoding.
- Image hosting.
- Restoring entries from the recycle bin.

## Upload preparation

### Default behavior

- Do not advertise `PutStream`.
- Use a source-provided MD5 without buffering when available.
- If the source does not provide an MD5:
  - Buffer small known-size inputs in memory.
  - Buffer large known-size inputs in a temporary file.
  - Buffer unknown-size inputs in a temporary file.
- Calculate the MD5 and, when necessary, the size while buffering.
- Control the memory-to-disk threshold with an advanced `hash_memory_limit`
  option.
- Clean up temporary files after success, failure, or cancellation.

### `no_buffer` behavior

- Provide an advanced `no_buffer` option in the initial release.
- When the source does not provide an MD5:
  - Read the source once to calculate its MD5.
  - Reopen the source from the beginning.
  - Read it again for the actual upload.
- Return a clear error when the source cannot be reopened.
- Do not silently fall back to local buffering when `no_buffer` is enabled.
- Unknown-size `rclone rcat` input is still spooled by rclone before `Put`,
  because the backend does not advertise `PutStream`.

### Constraints

- 123Pan requires the complete file size and MD5 before it creates an upload
  session.
- `no_buffer` saves local storage at the cost of reading the source twice.
- Reading a remote source twice can double its download traffic.
