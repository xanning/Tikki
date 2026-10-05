# Arbitrary File Smuggling via Image CDN in TikTok Ads Manager Image Upload.

## Summary

TikTok Ads Manager's creative image upload endpoint

```
POST https://ads.tiktok.com/mi/api/v2/i18n/material/image/upload/
```

skips server-side re-encoding for images uploaded at specific fixed dimensions (the documented Global App Bundle creative-size minimums: **640×640**, **1280×720**, **720×1280**). When an uploaded PNG matches one of these exact dimensions, the file is stored and served **byte-for-byte** through a long-lived, signed CDN URL — including any bytes appended after the PNG's `IEND` chunk.

Because PNG decoders stop reading at `IEND`, a file can be constructed that is simultaneously:
- a 100% valid, decodable PNG image (satisfying any client-side/server-side "is this an image" check), and
- a container for an arbitrary secondary payload of any size, appended after the image data and never touched by re-encoding.

This allows an authenticated advertiser (a free/unfunded ad account is sufficient — no spend or campaign approval required to reach the upload step) to host **arbitrary binary content** on TikTok's own trusted CDN infrastructure, disguised as an ad creative image, indefinitely (the signed URL's validity has been observed to be on the order of a year).


---

# Mirror tool (Tikki)

Takes a source HLS stream (master playlist with separate video + audio-group renditions) and mirrors every segment onto TikTok's own CDN as polyglot PNGs, then serves back a **standard, spec-compliant** HLS playlist (byte-range based — no custom player code needed) pointing at the mirrored copies.

## Running it as a service

Deploy layout (same on every box this has been run on):

```
/opt/tikki/tikki           - the compiled binary
/opt/tikki/session.txt     - session/auth material (see Session below)
/var/lib/tikki/playlists/  - generated playlists, one dir per job id, persists across reboots
/var/log/tikki/            - service.log (stdout/stderr) + mirror-debug.log (per-job progress trace)
```

Build (cross-compile from anywhere with Go):

```bash
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -o tikki-linux
```

systemd unit (`/etc/systemd/system/tikki.service`):

```ini
[Unit]
Description=Tikki - TikTok CDN HLS Mirror API
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
ExecStart=/opt/tikki/tikki -addr 127.0.0.1:8787 -session /opt/tikki/session.txt -concurrency 8
WorkingDirectory=/opt/tikki
Restart=always
RestartSec=5
StandardOutput=append:/var/log/tikki/service.log
StandardError=append:/var/log/tikki/service.log
User=root

[Install]
WantedBy=multi-user.target
```

```bash
mkdir -p /opt/tikki /var/lib/tikki/playlists /var/log/tikki
# scp the binary + session.txt into /opt/tikki, then:
systemctl daemon-reload
systemctl enable --now tikki
curl -s http://127.0.0.1:8787/health
```

The binary runs directly under systemd — there is no wrapper script, and it does **not** submit any job on its own. It just sits there as an always-on API; you submit jobs to it whenever you want, as many as you want, over its lifetime. `Restart=always` means it survives crashes/reboots with no manual intervention.

Flags: `-addr` (listen address, default `127.0.0.1:8787`), `-session` (path to session file, default `session.txt`), `-concurrency` (default concurrent segment uploads, default `8`, overridable per-job — see below).

## Session

The uploader needs to look like a real logged-in browser hitting the TikTok Ads Manager image-upload endpoint. **Getting the material:**

1. Open `https://ads.tiktok.com/i18n/creative-library/dashboard`
2. Click "Upload creative", upload literally any random PNG
3. While it's uploading, open devtools → Network tab, find the request to
   `https://ads.tiktok.com/mi/api/v2/i18n/material/image/upload/`
4. Right-click it → **Copy → Copy Request Headers** (or your browser's equivalent — this copies the full raw request, including the `POST ... HTTP/2` request line, `Host`, `Cookie`, `Referer`, `Origin`, `User-Agent`, `X-CSRFToken`, etc.)
5. Paste that verbatim into `session.txt`. Nothing to reformat — the loader parses the raw dump directly.

Example shape (`session.txt`):

```
POST /mi/api/v2/i18n/material/image/upload/?aadvid=...&msToken=...&X-Bogus=...&X-Gnarly=... HTTP/2
Host: ads.tiktok.com
User-Agent: Mozilla/5.0 (...)
Referer: https://ads.tiktok.com/i18n/creative-library/dashboard?aadvid=...
X-CSRFToken: ...
Origin: https://ads.tiktok.com
Cookie: ttwid=...; sessionid=...; msToken=...; (the whole cookie jar, semicolon-separated)
```

`loadSession` auto-detects this format (file starts with `POST `/`GET `/`PUT `) and pulls out: full upload URL (`Host` + request-line path+query), `Cookie`, `X-CSRFToken`, `Referer`, `Origin`, `User-Agent`. A legacy hand-built JSON shape is still accepted as a fallback if the file doesn't start with an HTTP method:

```json
{
  "upload_url": "https://ads.tiktok.com/mi/api/v2/i18n/material/image/upload/?...",
  "cookie": "...",
  "csrf_token": "...",
  "referer": "...",
  "origin": "...",
  "user_agent": "..."
}
```

The session (cookies/msToken/X-Bogus/X-Gnarly) expires — when `GET /health` starts showing `session_ready: false` or uploads start 403ing, repeat the steps above and overwrite `session.txt`. No restart needed — it's re-read from disk on every upload.

## API

### `GET /health`
```json
{"ok": true, "session_ready": true, "png_offset": 2963, "session_message": "ready"}
```

### `POST /submit/mirror`
Request body:
```json
{
  "playlist_url": "https://source.example/master.m3u8",
  "concurrency": 16
}
```
- `playlist_url` — the source **master** playlist. Tikki automatically picks the 1080p AVC+AAC video variant and resolves its matching alternate-audio `EXT-X-MEDIA` group (not just any variant mentioning `audio_aac` — it follows the real `AUDIO="..."` group reference).
- `concurrency` — optional, overrides the service's default `-concurrency` flag for this job only. **Keep this ≤16.** TikTok's upload endpoint rate-limits/WAF-bans bursts past that — concurrency 32 reliably triggered a wave of `403 Forbidden` mid-job (segments silently dropped, job still reports "completed" with a short count). 8-16 has run clean every time.

Response: `202 Accepted`, `{"job_id": "<id>"}`.

### `GET /status/{jobID}`
Poll this while a job runs. Shape:
```json
{
  "ID": "...", "Status": "running|completed|failed",
  "Uploaded": 188, "Total": 320,
  "Error": "",
  "Result": {
    "playlist": "...",        // master playlist text
    "video_playlist": "...",
    "audio_playlist": "...",
    "playlist_url": "http://127.0.0.1:8787/playlists/<jobID>/master.m3u8",
    "init": { "url": "...", "byterange": "782@2963", ... },
    "audio_init": { ... },
    "segments": [ { "url": "...", "size": 4111668, "byterange": "4111668@2963", "duration": 10.427, "index": 0 }, ... ],
    "audio_segments": [ ... ],
    "elapsed": "42.3s"
  }
}
```
`Result` fills in live as segments finish (not just at completion) — `Uploaded`/`Total` and the `segments`/`audio_segments` arrays grow as the job progresses, so you can poll this for real-time progress rather than just a final result. `Total` counts video + audio segments combined (they're independently segmented, so audio usually has a different count than video — this is normal, not a bug).

### `GET /playlists/{jobID}/{file}`
Serves the finished playlist straight off disk once the job completes. `{file}` is one of `master.m3u8`, `video.m3u8`, `audio.m3u8` — nothing else is served (strict allowlist, no path traversal). This is the URL `Result.playlist_url` points at.

### `POST /upload/segment/`
Raw PoC endpoint — send any binary body, get back the TikTok CDN URL it was mirrored to. Used internally by the mirror pipeline; also handy for one-off smoke-testing that the session is alive (`curl --data-binary @file.bin .../upload/segment/`).

## What it actually produces

Three files per job, written to `/var/lib/tikki/playlists/<jobID>/` and served locally:

- **`master.m3u8`** — the entry point. Standard multivariant playlist: one `#EXT-X-STREAM-INF` (real `CODECS` string read from the source, not guessed) + one `#EXT-X-MEDIA:TYPE=AUDIO` pointing at `audio.m3u8`.
- **`video.m3u8`** / **`audio.m3u8`** — each a normal VOD media playlist. The init segment and every media segment are referenced via `#EXT-X-MAP:...,BYTERANGE=` / `#EXT-X-BYTERANGE:<len>@<offset>` pointing straight at the real ISOBMFF bytes *inside* the polyglot PNG object on TikTok's CDN (confirmed TikTok's CDN, fronted by Akamai, honors `Range` requests and returns clean `206 Partial Content` with zero PNG bytes in the body).

Because it's plain byte-range HLS, **any standard player works with zero custom code** — Safari native HLS, ExoPlayer, VLC, mpv, vanilla hls.js. No PNG-aware fragment loader needed (an earlier version of this used one; it's gone now that byte-range does the same job the standard way).

Segment durations are read straight from the source playlist's own `#EXTINF` values, not re-derived — probing duration from an isolated `moof`/`mdat` fragment with ffprobe (no `moov` context) silently fails and was previously causing every segment to report a flat, wrong `10.000s`, which is what caused MSE buffer/seek errors during playback. Fixed by just trusting the source's own numbers.

## Vulnerability Class

- CWE-434: Unrestricted Upload of File with Dangerous Type
- CWE-436: Interpretation Conflict (the file is validated/handled as "just an image" by TikTok's pipeline, but is a polyglot with a second, distinct payload)

## Affected Component

- Endpoint: `POST /mi/api/v2/i18n/material/image/upload/` (TikTok Ads Manager, `ads.tiktok.com`)
- Trigger condition: uploaded image's pixel dimensions exactly match one of the platform's documented minimum creative sizes (640×640 / 1280×720 / 720×1280)

## Root Cause

Standard practice for any platform accepting user-uploaded images is to decode and **re-encode** the image server-side before storage — this is what strips metadata, normalizes format, and (as a side effect) destroys any non-image data appended to the file. TikTok's pipeline appears to skip this re-encode step specifically when the uploaded image already exactly matches one of the platform's documented spec-minimum dimensions (presumably an optimization: "this is already the exact size we'd resize it to, so don't bother"). At those specific dimensions, the file is instead stored and served as uploaded, with no byte-level validation that the file *is* just image data and nothing more.

## Proof of Concept

1. Construct a minimal, fully valid PNG at exactly 640×640 (or 1280×720 / 720×1280):
   - PNG signature (8 bytes)
   - `IHDR` chunk declaring the target width/height, 8-bit RGBA
   - `IDAT` chunk (a single solid-color frame is sufficient — zlib-deflated raw pixel data)
   - `IEND` chunk (zero-length, per spec)

   This produces a small, deterministic, always-identical header (3043 bytes for a solid-white 640×640 RGBA frame using minimal zlib compression).

2. Append an arbitrary secondary payload directly after the `IEND` chunk's bytes — no PNG chunk structure required for this part, since decoders never read past `IEND`. For testing, any distinguishable byte sequence works (we used a short arbitrary binary blob with a recognizable marker to make integrity verification trivial).

3. Upload the resulting file through the normal ad-creative image upload flow in TikTok Ads Manager (any draft ad, no need to launch/spend), as a standard `multipart/form-data` POST with `Content-Type: image/png`, authenticated with a normal logged-in session (cookie + CSRF token — no special API access).

4. The response contains a CDN URL. Confirm the exploit two ways:
   - The URL renders as a normal, valid image when opened directly — visual/functional confirmation nothing was corrupted or rejected.
   - `curl`-fetching the URL returns a file whose length equals `header_length (3043) + payload_length` exactly, and the bytes from offset 3043 onward are **byte-for-byte identical** to the original appended payload — confirmed via direct diff against the source payload.

5. Repeated across multiple uploads/sessions to confirm the behavior is consistent, not a one-off caching artifact: dimensions matching the spec minimum reliably skip re-encoding; test uploads at non-matching dimensions (e.g. 641×641) were, by contrast, visibly re-processed (different output file size/hash than the input), consistent with normal re-encoding.

### Minimal reference implementation

A small Go proof-of-concept service is included in this repository:

- `polyglot.go` — builds the deterministic 640×640 white-canvas PNG header and appends an arbitrary payload after it.
- `tiktok.go` — performs the authenticated multipart upload to the endpoint above and extracts the served CDN URL from the response.
- `session.go` — loads auth material for the uploader (see **Session** below).
- `mirror.go` — the actual exploit payload: takes an arbitrary source HLS stream and re-hosts every segment on TikTok's CDN as a disguised polyglot, then rebuilds a standards-compliant HLS playlist pointing at the mirrored copies via `#EXT-X-BYTERANGE` (see **Mirror tool** below).
- `main.go` — the HTTP API exposing all of this (`POST /upload/segment/`, `POST /submit/mirror`, `GET /status/{jobID}`, `GET /playlists/{jobID}/{file}`, `GET /health`).

## Human

Basically the whole vietnamese streaming community use(d) this, this is a PoC of it, but i didn't test it in mass scale. Basically this though.
