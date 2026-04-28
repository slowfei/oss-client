# direct_grant_qiniu

This example demonstrates the **non-URL DirectGrant pattern** that Qiniu Upload
Token requires. The v0.1 frozen `DirectGrantMode` set covers it via
`Mode=DirectGrantModeToken`: the upload authorization is an opaque bearer string
the caller POSTs as a multipart form field — it is not a URL, not a set of
form-data fields by themselves, and not a custom HTTP header.

This is the M5 milestone validation moment: `DirectGrantModeToken` exercised in
a _new_ context distinct from Azure SAS (which is also `Mode=Token` but encoded
as a URL query string handed to the storage endpoint). Qiniu's token is POSTed
to a vendor-specific upload host as the `token` form field.

The download path (Operation=download) is covered too. Per the v0.1.1 patch,
Qiniu's download authorization is a signed URL, so `IssueDirectGrant` returns
`Mode=DirectGrantModeURL` for that case — callers GET `DirectGrant.URL` directly.

## Run

```bash
# uses placeholder credentials by default — output is structurally correct
# but the Token will not authenticate against a real Qiniu bucket
cd examples/direct_grant_qiniu
go run .
```

For a real Qiniu bucket, set the env vars first:

```bash
export OMC_QINIU_DEMO_KEY=<your-access-key>
export OMC_QINIU_DEMO_SECRET=<your-secret-key>
export OMC_QINIU_DEMO_BUCKET=<your-bucket-name>
export OMC_QINIU_DEMO_DOMAIN=https://<your-bucket-domain>
export OMC_QINIU_DEMO_REGION=z0   # z0=华东, z1=华北, z2=华南, na0=北美, as0=东南亚

go run .
```

`OMC_QINIU_DEMO_DOMAIN` is the CDN/source domain bound to your Qiniu bucket.
It is **required** for `Operation=download` (the driver calls
`storage.MakePrivateURL` which needs the domain). For upload-only usage it can
be any placeholder string.

If the env vars are unset the example falls back to placeholders
(`DEMO_AK` / `DEMO_SK` / `demo-bucket` / `https://demo.example.com`). The
generated Upload Token is cryptographically valid HMAC-SHA1 but will be rejected
by a real Qiniu endpoint — which is fine because this example is about showing
the **shape** of the grant, not executing a live upload.

## Expected output

```
# [placeholder mode] No OMC_QINIU_DEMO_* env vars set.
# The Upload Token below is structurally correct but won't
# authenticate against a real Qiniu bucket.
#
opened qiniu client  bucket=demo-bucket  region=z0  domain=https://demo.example.com

=== Upload Token (DirectGrantModeToken) ===
  Mode      : token
  URL       : https://upload.qiniup.com
  Method    : POST
  Token     : DEMO_AK:HMAC_SHA1_SIGNATURE=:BASE64_PUT_POLICY=
  ExpiresAt : 2026-04-28T23:30:00Z

=== How a caller uses the Upload Token (curl) ===
  curl -X POST 'https://upload.qiniup.com' \
       -F token='DEMO_AK:HMAC_SHA1_SIGNA...' \
       -F key='uploads/demo-image.jpg' \
       -F file=@/path/to/local/image.jpg

  # The token field is the ONLY authorization credential.
  # No Authorization header. No query-string signature.
  # This is the Mode=Token dispatch shape: opaque bearer
  # string carried in vendor-defined form field.

=== Download URL (DirectGrantModeURL) ===
  Mode      : url
  URL       : https://demo.example.com/uploads/demo-image.jpg?e=1745882...&token=DEMO_AK:...
  Method    : GET
  ExpiresAt : 2026-04-28T23:30:00Z

=== How a caller uses the Download URL (curl) ===
  curl -X GET 'https://demo.example.com/uploads/demo-image.jpg?e=174...'

  # Mode=url: the signed URL IS the grant.
  # No additional headers or form fields needed.

=== Caller-side dispatch on grant.Mode ===
  Mode=token  → POST to https://upload.qiniup.com with form field token=<Token>
  Mode=url    → GET https://demo.example.com/uploads/demo-image.jpg?e=1... directly (URL IS the grant)

direct_grant_qiniu OK
```

## What this demonstrates

| Aspect | Detail |
|---|---|
| **Upload Token shape** | `Mode=Token`, `Token=<opaque-string>`, `URL=<upload-host>`, `Method=POST`. Caller POSTs multipart/form-data with `token=<Token>` field to `URL`. |
| **Download URL shape** | `Mode=URL`, `URL=<signed-private-URL>`, `Method=GET`. Caller GETs `URL` directly (v0.1.1 patch — architecturally honest encoding). |
| **PutPolicy Extra knobs** | 8 override keys forwarded into Qiniu's PutPolicy: `callbackUrl`, `callbackBody`, `callbackHost`, `callbackBodyType`, `returnBody`, `returnUrl`, `saveKey`, `persistentOps`. |
| **Cross-reference: Azure SAS** | Azure SAS is also `Mode=Token` but the token is a URL query string appended to the storage endpoint. Both dispatch as `Mode=Token`; the wire shape differs per vendor — the caller need not branch. |
| **Placeholder-safe** | The example runs without real credentials. The HMAC-SHA1 token is structurally valid; it just won't pass Qiniu's auth check. |

## Why this matters

The unified `Signer.IssueDirectGrant` API lets business code support both
AWS-presigned-URL semantics (`Mode=URL`) and Qiniu-token semantics
(`Mode=Token`) **without vendor-specific branching**. The caller dispatches on
`grant.Mode` and applies the right HTTP mechanic: GET the URL (Mode=URL), POST
form fields (Mode=Form), attach headers (Mode=Headers), or carry an opaque
bearer token (Mode=Token). The four frozen `DirectGrantMode` values cover every
wire shape known at v0.1, and adding a fifth requires a freezing-process re-run
— surface_test.go `TestFrozenSurface/direct_grant_modes_frozen_4` enforces this
invariant on every CI run.

## See also

- [`providers/qiniu/README.md`](../../providers/qiniu/README.md) — driver-level
  docs covering the three Qiniu token families (Upload, Download, Manage) and
  the `DriverConfig` field reference.
- [`docs/provider_matrix.md`](../../docs/provider_matrix.md) — cross-provider
  capability matrix; footnote 4 explains why Qiniu's write path is non-URL.
- [`examples/quickstart/`](../quickstart/) — end-to-end demo covering Put / Get
  / SignURL / Capabilities; does not exercise `IssueDirectGrant`.
