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
- `main.go` — exposes this as `POST /upload/segment/` for repeatable testing.

Session/auth material (cookies, CSRF token) is I didn't bother making it automated — it's supplied at runtime via a local, gitignored `session.json` (see `session.example.json` for the shape), captured by hand from a real logged-in browser session and refreshed manually when it expires.

## Human

Basically the whole vietnamese streaming community use(d) this, this is a PoC of it, but i didn't test it in mass scale. Basically this though.