# o2-rclone

**Native O2 Cloud backend for rclone** — connects directly to O2 Cloud's Funambol MediaHub SAPI API without any gateway or intermediary process.

## What this does

Adds a new `o2` backend to rclone, allowing you to mount, read, write, and manage your O2 Cloud files directly:

```bash
rclone lsd o2:
rclone copy ./my-files o2:backups/
rclone mount o2: /Volumes/O2Cloud --vfs-cache-mode writes
rclone about o2:
```

## How it works

The backend reverse-engineers the O2 Cloud API (Funambol MediaHub SAPI v31):

- **Auth**: `Authorization: oauth <bundle>` + `validationkey` query param + `X-deviceid`/`X-devicename` headers + `JSESSIONID` cookie
- **Renewal**: `POST /sapi/login/oauth?action=login` with the oauth bundle returns rotated tokens — fully automatic via a keepalive goroutine
- **No browser needed**: Once the initial session is captured, the backend renews itself indefinitely

## Quick start

### 1. Build the custom rclone

```bash
# Clone this repo
git clone https://github.com/ajoms/o2-rclone.git
cd o2-rclone

# Build (requires Go 1.24+)
chmod +x build.sh
./build.sh

# Binary at ./build/rclone
./build/rclone version   # should show rclone with "o2" backend
```

### 2. Capture the initial session

The backend needs a session with `validation_key`, `oauth_bundle`, and `JSESSIONID` cookies. Two methods:

#### Option A: Via the O2 Cloud gateway (automated)

If you have [o2cloud_gateway_webdav](https://github.com/garanda21/o2cloud_gateway_webdav) running, use the session helper:

```bash
python3 backend/o2/o2-reauth.py --force
```

This triggers the gateway's browser login, captures the session, and updates your rclone config.

#### Option B: Capture manually with mitmproxy

1. Install mitmproxy: `brew install mitmproxy`
2. Start it: `mitmdump -p 8080 -s backend/o2/o2_reauth_capture.py`
3. Launch the O2 Cloud app with the proxy: `https_proxy=http://127.0.0.1:8080 O2\ Cloud`
4. The captured session is saved to `o2_session.json`
5. Create the rclone remote:

```bash
rclone config create o2 o2   validation_key="<from capture>"   oauth_bundle="<from capture>"   cookie_jsessionid="<from capture>"   device_id="<from capture>"   keepalive_seconds=240
```

### 3. Use it

```bash
rclone lsd o2:                    # list root
rclone lsf o2:Photos/             # list files
rclone copy /local o2:backup/     # upload
rclone mount o2: /Volumes/O2      # mount (needs macFUSE on macOS)
rclone about o2:                  # quota info
```

## Configuration options

| Option | Default | Description |
|--------|---------|-------------|
| `validation_key` | *required* | Session validation key |
| `oauth_bundle` | *required* | OAuth2 access token bundle (base64-encoded JSON) |
| `cookie_jsessionid` | | JSESSIONID cookie |
| `device_id` | `O2CloudRclone` | Device identifier for the API |
| `device_name` | `O2 Cloud` | Device display name |
| `user_agent` | `O2CloudGateway/0.1` | User-Agent header |
| `api_url` | `https://cloud.o2online.es/sapi/` | SAPI API base URL |
| `upload_url` | `https://upload.cloud.o2online.es/sapi/` | Upload API URL |
| `keepalive_seconds` | `240` | Interval between auto-renewal pings (0 to disable) |

## Auto-renewal

The backend includes a keepalive goroutine (enabled by `keepalive_seconds`). Every N seconds it calls `POST /sapi/login/oauth?action=login` which returns fresh tokens. This means **the session never expires** without manual intervention.

For `rclone mount`, the keepalive runs in the background. For short commands (`lsd`, `copy`), the renewal happens only on 401 errors.

> **Note (oauth_bundle vs web login):** Fully silent renewal (no SMS) requires the session
> to carry an `oauth_bundle`, which only the native-app/mitmproxy capture produces (see
> "Alternative: capture session from the native O2 app" at the bottom). Sessions captured
> through the gateway/browser login (Option A) have an empty `oauth_bundle`, so the keepalive
> cannot rotate them: when that session expires you must re-validate via the web login
> (`o2-reauth.py`), which opens the O2 login page in Chromium (no native app involved).

## Limitations

- **Max file size: ~4-5 GB (server-enforced, hard limit).** Confirmed
  empirically: 4 GiB uploads are accepted; 5 GiB and larger are rejected with
  `MED-1020: File size exceeds configured max file size limit`. This is a
  server-side policy. Chunked uploads do NOT bypass it, because the total file
  size is declared in the metadata and rejected before any data transfer.
  Files larger than the limit (e.g. 20-60 GB) must be split client-side or
  stored on another provider.
- ~10,000 folders (O2 Cloud service limit)
- No native file hashes — rclone uses size + mtime for comparison
- Eventual consistency: ~2-5s delay after upload before file appears in listing

## Files

```
backend/o2/
  o2.go                   — Backend registration, Fs, Object, Config wizard
  client.go               — SAPI HTTP client, auth, renewal, keepalive, uploads
  login.go                — `authorize` command + session capture
  o2_authorize.py         — Playwright-based interactive login helper (web validation)
  o2-reauth.py            — Session check + auto-renewal (triggers the gateway web login)
  o2-reauth.py.notify     — Same as o2-reauth.py + macOS notifications, exponential backoff
                             and tri-state session check (recommended variant)
  o2_read_gateway_session.py — reads the fresh session from the gateway store (required by
                             o2-reauth.py)
  o2_reauth_capture.py    — mitmproxy addon: captures oauth_bundle from the native O2 app
  apply_o2_session.py     — applies a captured o2_session.json to rclone.conf
  capture-o2-session.sh   — orchestrates the mitmproxy capture with the native O2 app
```

## Session renewal (web login)

`o2-reauth.py` (and the `.notify` variant) is meant to run on a schedule (e.g. every 10 minutes
via launchd). It:

1. Checks the `o2native` session with a lightweight SAPI call.
2. When expired, triggers the gateway (`http://127.0.0.1:8088/api/admin/o2/login`) which opens
   the O2 web login in Chromium via Playwright. **No native O2 app involved.**
3. Waits for you to validate with SMS, then writes the fresh `validation_key` and
   `cookie_jsessionid` back to `rclone.conf`.

Differences between the two variants:

| Feature | `o2-reauth.py` | `o2-reauth.py.notify` |
|---|---|---|
| macOS notification on expiry / success / failure | — | ✅ |
| Exponential backoff (30 min → 1 h → 2 h) | — | ✅ |
| Ignores transient network errors (tri-state check) | — | ✅ |
| Avoids duplicate login windows | — | ✅ |

To switch to the improved variant:
`cp backend/o2/o2-reauth.py.notify backend/o2/o2-reauth.py` (keep the original as backup) and
restart the scheduler if needed.

## Backing up large files (4-60 GB movies)

O2 Cloud rejects files larger than ~4-5 GB (`MED-1020`). To back up a movie
library (e.g. an encrypted rclone crypt library), wrap the `o2` remote with
rclone's **chunker** backend, which transparently splits large files into
chunks under the limit and reassembles them on read:

```
o2            (native backend)
  └─ o2chunk   (chunker: remote=o2native:, chunk_size=3.5G)
       └─ o2media (crypt: remote=o2chunk, your password)   ← use this
```

```bash
# One-time setup
rclone config create o2chunk chunker remote=o2native: chunk_size=3.5G
rclone config create o2media crypt remote=o2chunk   password="$(rclone obscure 'YOUR_PASS')" salt="$(rclone obscure 'YOUR_SALT')"

# Backup (movie appears as ONE file on o2media, stored as ≤3.5G chunks on O2)
rclone copy /path/movies o2media:backups

# Restore to any local/shared-drive location (reassembles + decrypts)
rclone copy o2media:backups /path/restore
```

Notes:
- `transactions=norename` is required: O2 rejects `save-metadata` renames
  during the ~10s media validation window (MED-1017), so chunks keep their
  temp transaction suffix and the `simplejson` meta file records the xactID
  used to locate them on read.
- `chunk_size=3.5G` keeps each chunk safely under the server's ~4-5 GB cap.
- The crypt layer wraps the chunker, so each stored chunk is a raw ciphertext
  fragment — encrypted at rest, reassembled + decrypted on read.
- The O2 web/app will show the chunk files, not the original movie — this is
  a backup target; playback works through rclone mount (reassembly is
  transparent).

### Playing movies in Plex (streaming from O2)

```bash
# macOS: install macFUSE once, then mount
brew install --cask macfuse
rclone mount o2chunk: /Volumes/O2Cloud \
  --vfs-cache-mode full --dir-cache-time 60s --volname "O2Cloud"

# Point Plex at /Volumes/O2Cloud — it sees the full movie files.
# rclone streams the chunks on demand (full cache mode buffers locally for
# smooth playback).
```

No-macFUSE alternative (Finder-only, no Plex): `rclone serve webdav o2chunk: --addr 127.0.0.1:8099` then Finder Cmd+K to `http://127.0.0.1:8099/`.

## License

This project is based on [rclone](https://github.com/rclone/rclone) (MIT) and reverse-engineered from the O2 Cloud API using the open-source [o2cloud_gateway_webdav](https://github.com/garanda21/o2cloud_gateway_webdav) (MIT) as reference.

## Alternative: capture session from the native O2 app (mitmproxy backup)

If you prefer to avoid the browser login and instead capture a renewable session directly from
the O2 Cloud desktop app (one-time SMS login inside the app), use the mitmproxy helper:

1. Install the O2 Cloud app and trust the mitmproxy CA once:
   `sudo security add-trusted-cert -d -r trustRoot -k /Library/Keychains/System.keychain ~/.mitmproxy/mitmproxy-ca-cert.pem`
2. Run: `backend/o2/capture-o2-session.sh`

It starts `mitmdump` with `o2_reauth_capture.py`, opens the app through the proxy, waits for
`o2_session.json`, and applies the session with `apply_o2_session.py` (sets `validation_key`,
`oauth_bundle` and `cookie_jsessionid` on the `o2native` remote).

This is kept as a backup approach; the default is the web-validation flow (rclone + gateway
browser login). `o2_read_gateway_session.py` is a runtime dependency of `o2-reauth.py`.
