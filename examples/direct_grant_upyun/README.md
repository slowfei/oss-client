# direct_grant_upyun

Upyun FORM upload authorization via `Signer.IssueDirectGrant` — the M5
validation moment for `DirectGrantModeForm`, the **last** of the 4 frozen
`DirectGrantMode` values exercised in production. Unlike REST-PUT, Upyun
upload authorization is FORM-based: the client POSTs a `multipart/form-data`
payload carrying a base64-encoded JSON policy and a signed `authorization`
field to `https://v0.api.upyun.com/<service>`. `IssueDirectGrant` returns a
`*uos.DirectGrant{Mode: DirectGrantModeForm}` describing every field the
caller needs — no Upyun SDK dependency on the upload client.

## Run

### With real credentials

```bash
export OMC_UPYUN_DEMO_BUCKET=my-service-name    # Upyun "service name" (= bucket)
export OMC_UPYUN_DEMO_OPERATOR=my-operator       # service-scoped operator name
export OMC_UPYUN_DEMO_PASSWORD=my-operator-password  # plaintext; SDK MD5s it

go run .
```

`OMC_UPYUN_DEMO_BUCKET` is the Upyun **service name** — the 1:1 mapping of
the unified `Bucket` concept onto Upyun's namespace. Services are provisioned
via the [Upyun console](https://console.upyun.com/); there is no programmatic
create-service API (the driver returns `ErrUnsupported` for `BucketService.Create`).

### With placeholders (structural demo)

```bash
go run .
```

No cloud account needed. Placeholder credentials produce a structurally valid
grant whose HMAC-SHA1 signature is computed over the placeholder password. The
printed fields are real (base64 policy, signed authorization header) — they
just won't be accepted by real Upyun endpoints.

## Expected output

```
NOTE: using placeholder credentials — grant signature is structurally valid but not accepted by real Upyun.
      Set OMC_UPYUN_DEMO_BUCKET, OMC_UPYUN_DEMO_OPERATOR, OMC_UPYUN_DEMO_PASSWORD for live validation.

opened upyun client → service=my-upyun-service operator=demo-operator

=== DirectGrant (Mode=Form, Operation=Upload) ===
  Mode:      form
  URL:       https://v0.api.upyun.com/my-upyun-service
  Method:    POST
  ExpiresAt: 2026-04-28T23:30:00Z

  Headers:
    Authorization: UpYun demo-operator:XXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXX...

  FormFields:
    authorization: UpYun demo-operator:XXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXX...
    policy: eyJhbGxvdy1maWxlLXR5cGUiOiJpbWFnZS9qcGVnIiwiYnVja2...

=== Equivalent curl command ===
  curl -F "policy=eyJhbGxvdy1maWxlLXR5cGUiOiJpbWFnZS9qcGVnIiwiYnV..." \
       -F "authorization=UpYun demo-operator:XXXXXXXXXXXXXXXX..." \
       -F "file=@local.jpg" \
       https://v0.api.upyun.com/my-upyun-service

=== Mixed Signer dispatch: Download via SignURL ===
  IssueDirectGrant(download) → ErrUnsupported (capability=signer.direct_grant)
  Message: upyun download authorization is URL-shaped; use Signer.SignURL(method=GET) for download grants

  SignURL(GET) → https://my-upyun-service.b0.upaiyun.com/uploads/2026/photo...
  ExpiresAt:    2026-04-28T23:30:00Z

direct_grant_upyun OK — see README.md for the full educational story
```

## What this demonstrates

| Scenario | Mode | FormFields | Headers | Method | URL |
|---|---|---|---|---|---|
| FORM upload grant | `form` | `policy`, `authorization` (+ optional `content-md5`, `x-upyun-meta-*`) | `Authorization` | `POST` | `https://v0.api.upyun.com/<service>` |
| Download — wrong path | — | — | — | — | `ErrUnsupported{CapDirectGrant}` → use `SignURL` |
| Download — correct path | URL via `SignURL(GET)` | — | — | `GET` | CDN URL with `_upt=<sig>/<expiry>` |

### The 6 vendor-specific Extra keys Upyun recognises

| `Extra` key | Policy / form field | Purpose |
|---|---|---|
| `"notify-url"` | `policy["notify-url"]` | Async callback URL after upload |
| `"apps"` | `policy["apps"]` | JSON-encoded pre-treatment array |
| `"expiration-override"` | `policy["expiration"]` | Unix seconds; overrides `ExpiresIn` |
| `"save-key"` | `policy["save-key"]` | Object path override (supports templates) |
| `"content-md5"` | form-field `content-md5` | Whole-object MD5 integrity check |
| `"allow-file-type"` | `policy["allow-file-type"]` | Content-type restriction override |

All other keys are silently ignored (lenient contract).

## Comparison: Qiniu vs Upyun DirectGrant

The same `*uos.DirectGrant` struct absorbs two very different vendor shapes
via `Mode` dispatch:

| Field | Qiniu (Token) | Upyun (Form) |
|---|---|---|
| `Mode` | `DirectGrantModeToken` (`"token"`) | `DirectGrantModeForm` (`"form"`) |
| `Token` | opaque upload token string | `""` (not used) |
| `FormFields` | `nil` | `{"policy": "...", "authorization": "..."}` |
| `Headers` | `nil` | `{"Authorization": "UpYun op:sig"}` |
| `Method` | `POST` (to qiniu upload endpoint) | `POST` (to `v0.api.upyun.com/<svc>`) |
| `URL` | Qiniu upload endpoint | `https://v0.api.upyun.com/<service>` |
| Client dispatch | `Authorization: UpToken <token>` header | FORM fields in multipart body |

Business code dispatches purely on `grant.Mode` — no vendor-specific `if`
branches needed:

```go
switch grant.Mode {
case uos.DirectGrantModeToken:
    // Qiniu: pass grant.Token as Authorization: UpToken <token>
case uos.DirectGrantModeForm:
    // Upyun: POST grant.FormFields as multipart/form-data to grant.URL
case uos.DirectGrantModeURL:
    // Azure SAS: redirect/link to grant.URL directly
case uos.DirectGrantModeHeaders:
    // signed-headers flow: attach grant.Headers to a PUT to grant.URL
}
```

## Why this matters

The v0.1 frozen 4-mode set is now **proven end-to-end**:

| Mode | Provider | Status |
|---|---|---|
| `url` | Azure SAS | Validated |
| `token` | Qiniu Upload Token | Validated |
| `headers` | Signed-headers flow | Validated |
| `form` | **Upyun FORM upload** | **Validated here (M5 moment)** |

`DirectGrant.FormFields map[string]string` absorbs Upyun's FORM shape cleanly —
no new fields required, no v0.2.0 widening candidate. Business code dispatches
purely on `grant.Mode` without any vendor branching. The M5 validation verdict:
**the existing 4-field struct is sufficient**.

## See also

- [`providers/upyun/README.md`](../../providers/upyun/README.md) — driver
  internals, auth shapes, bucket→service mapping, multipart details
- [`examples/direct_grant_qiniu/README.md`](../direct_grant_qiniu/README.md) —
  companion example showing `DirectGrantModeToken` (Qiniu Upload Token)
- [`docs/provider_matrix.md`](../../docs/provider_matrix.md) — cross-provider
  capability matrix including footnote 3 (Upyun SignedURLWrite fence) and
  footnote 7 (Upyun metadata best-effort on FORM upload)
