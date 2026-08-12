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
  o2.go              — Backend registration, Fs, Object, Config wizard
  client.go          — SAPI HTTP client, auth, renewal, keepalive, uploads
  login.go           — `authorize` command + session capture
  o2_authorize.py    — Playwright-based interactive login helper
  o2-reauth.py       — Session check + auto-renewal helper
```

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
- `chunk_size=3.5G` keeps each chunk safely under the server limit (with the
  default `hash_type=md5` the chunk names carry a hash suffix).
- The chunks are stored as raw ciphertext fragments (the crypt layer wraps the
  chunker), so data is encrypted at rest and reassembled+decrypted on read.
- The O2 web/app will show the chunk files, not the original movie — this is
  a backup target, not a streaming library.

## License

This project is based on [rclone](https://github.com/rclone/rclone) (MIT) and reverse-engineered from the O2 Cloud API using the open-source [o2cloud_gateway_webdav](https://github.com/garanda21/o2cloud_gateway_webdav) (MIT) as reference.
