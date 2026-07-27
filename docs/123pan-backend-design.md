# 123Pan backend design

## Status

- Accepted and implemented.
- The backend follows OpenList's ordinary `drivers/123` protocol.
- The focused unit tests use the public rclone filesystem operations.
- The standard backend integration suite can be enabled with a configured
  `Test123Pan:` remote.

## Authentication

- Configure a 123Pan account `username` and `password`.
- Store the password with rclone's normal obscured-password option handling.
- Sign in through `https://login.123pan.com/api/user/sign_in`.
- Persist the returned access token as the sensitive `token` option.
- Reuse a persisted token in a later rclone process before signing in again.
- Do not configure a refresh token, token broker, token server, or
  rotating-token state for this backend.
- Validate submitted credentials during interactive configuration.
- Lazily establish the runtime session when the first authenticated operation
  needs it.
- On an ordinary API or HTTP 401:
  - Sign in once with the configured account credentials.
  - Retry the failed request once.
  - Return a second authentication failure without another login loop.

## Ordinary web API protocol

- Use `https://yun.123pan.com/b/api` as the API root, matching OpenList's
  ordinary driver.
- Send the normal web headers:
  - `Authorization: Bearer <persisted session token>`.
  - `Platform`, defaulting to `web`.
  - `App-Version: 3`.
  - OpenList-compatible `Origin`, `Referer`, and user-agent headers.
- Append OpenList's time-dependent CRC32 query signature to every authenticated
  request.
- Keep the `platform` header configurable as an advanced option.
- Treat the session token as a replaceable bearer credential.
- On a 401, sign in once and atomically replace the persisted token through
  rclone's normal config writer before retrying the failed request.

## Filesystem operations

- List with `GET /file/list/new`.
- Download with `POST /file/download_info`, then use the returned URL and its
  required referrer.
- Create a directory through `POST /file/upload_request` with `type: 1`.
- Rename with `POST /file/rename`.
- Move with `POST /file/mod_pid`.
- Send deletes to the recycle bin with `POST /file/trash`.
- Read capacity with `GET /user/info`.
- Return MD5 from the ordinary API `Etag` field.

## Server-side copy

- Start a copy with
  `POST /restful/goapi/v1/file/copy/async`.
- Include the source file identifier, parent identifier, size, ETag, type, and
  filename in `fileList`.
- Poll `GET /restful/goapi/v1/file/copy/task` until the task completes.
- Resolve the copied object from the destination directory after completion so
  rclone receives its actual new object identifier.
- Perform a normal rename afterwards when rclone requests a different target
  leaf name.

## Uploads and `no_buffer`

- Start uploads with `POST /file/upload_request` using the full-file MD5.
- Accept instant upload when the API reports content reuse.
- Upload non-reused bytes by either:
  - The temporary S3 credentials returned by the upload request.
  - The ordinary API's temporary presigned S3 URL flow.
- Complete the corresponding ordinary upload protocol after S3 transfer.
- Keep the existing `no_buffer` contract:
  - Calculate MD5 from an already-known hash when available.
  - Otherwise require a seekable or reopenable source.
  - Return an error instead of silently using an rclone memory or disk spool.
- Use the configured hash-memory limit and temporary file spool only when
  `no_buffer` is disabled and rclone has not supplied a whole-file MD5.

## Names and scope

- Encode 123Pan-disallowed file-name characters through rclone's standard
  encoder option.
- Convert names in both directions at listing, creation, upload, rename, move,
  copy, and download boundaries.
- Support list, download, upload, directory creation, recycle-bin delete,
  rename, move, server-side copy, MD5, and capacity usage.
- Exclude shares, offline download, and direct-link commands from this backend.
