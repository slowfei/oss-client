# providers/azure

`github.com/maqian/object-storage-client/providers/azure` — native driver for
**Azure Blob Storage** via [`github.com/Azure/azure-sdk-for-go/sdk/storage/azblob`](https://godoc.org/github.com/Azure/azure-sdk-for-go/sdk/storage/azblob).

| Field | Value |
| --- | --- |
| Module path | `github.com/maqian/object-storage-client/providers/azure` |
| Tag | `providers/azure/v0.1.1` |
| Vendor SDK | `github.com/Azure/azure-sdk-for-go/sdk/storage/azblob v1.6.4` |
| Provider id | `"azure"` (the `uos.Config.Provider` value) |
| AuthScheme | `AuthSharedKey` (account key), `AuthSAS` (pre-formed SAS token), `AuthCustom` (Entra ID / token credential) |
| Milestone | M4 |

## Install

```bash
go get github.com/maqian/object-storage-client/providers/azure@providers/azure/v0.1.1
```

## Quickstart

```go
package main

import (
    "context"
    "log"
    "strings"

    "github.com/maqian/object-storage-client/pkg/uos"
    "github.com/maqian/object-storage-client/pkg/uos/credential"
    azuredrv "github.com/maqian/object-storage-client/providers/azure"
    _ "github.com/maqian/object-storage-client/providers/azure" // registers Factory
)

func main() {
    ctx := context.Background()
    cfg := uos.Config{
        Provider: "azure",
        // Config.Region is NOT used — StorageAccount encodes the location.
        DriverConfig: &azuredrv.DriverConfig{
            StorageAccount: "mystorageaccount",
            // ServiceURL defaults to https://mystorageaccount.blob.core.windows.net/
        },
        CredentialProvider: credential.NewStatic(credential.Credential{
            Scheme: credential.AuthSharedKey,
            Opaque: &azuredrv.SharedKeyCredential{
                AccountName: "mystorageaccount",
                AccountKey:  "base64encodedkey==",
            },
        }),
    }
    cli, err := uos.DefaultRegistry().Open(ctx, cfg)
    if err != nil {
        log.Fatal(err)
    }
    defer cli.Close()

    _, err = cli.Objects("my-container").Put(ctx, uos.PutObjectRequest{
        Key:  "hello.txt",
        Body: strings.NewReader("hello world"),
        Size: int64(len("hello world")),
    })
    if err != nil {
        log.Fatal(err)
    }
}
```

## Provider-specific configuration

Azure Blob Storage organises objects under **Containers**, which the driver maps 1:1 to the unified `Bucket` concept. The **Storage Account** is a driver-level concept: unlike S3 where `Config.Region` is a separate field, the account name encodes the geographic location and forms the base URL (`https://<account>.blob.core.windows.net/`).

`*azure.DriverConfig` is **required** and accepts:

- `StorageAccount string` — **required** unless `ServiceURL` is set directly.
- `ServiceURL string` — overrides the auto-derived service URL. Use for Azurite (the local emulator), sovereign clouds (`*.blob.core.chinacloudapi.cn`), or custom private link endpoints.
- `APIVersion string` — pins the `x-ms-version` header. Empty uses the azblob SDK default (`"2024-11-04"`).

Three `AuthScheme` values are supported:

- `AuthSharedKey` — supply `*azure.SharedKeyCredential{AccountName, AccountKey}` in `Opaque`. The `AccountKey` is the base64-encoded key from the Azure portal (`az storage account keys list`).
- `AuthSAS` — supply `*azure.SASCredential{Token}` where `Token` is a pre-formed SAS query string (`sv=2022-11-02&ss=b&...`). Leading `"?"` is stripped automatically.
- `AuthCustom` — supply `*azure.TokenCredential{Credential: azcore.TokenCredential}` for Entra ID / service-principal auth. If `Opaque` is nil or unrecognised, falls back to `azidentity.NewDefaultAzureCredential` (ADC chain).

SAS start-time for signed URLs: the driver sets `start = now−5min` for clock-skew tolerance; `expiry = now+ExpiresIn`.

## Capability shape

Azure has a **distinctive column** — the only provider where `CapDirectGrant` is **Supported** (not Unsupported):

- `CapDirectGrant` is **Supported**: Azure SAS expressed as `DirectGrant{Mode: DirectGrantModeToken}`. The SAS query string is the opaque bearer token; the caller carries it and appends it as a query string to the blob URL.
- `CapSignedURLRead` and `CapSignedURLWrite` are **Conditional** (footnote 6): SAS generation requires `AuthSharedKey` or `AuthCustom`; `AuthSAS` callers lack key material.
- `CapObjectACL` is **Conditional** (footnote 11): Azure has no S3-style per-object ACL; access is controlled via SAS or RBAC.
- `CapVersioning` is **Conditional**: requires storage account-level versioning enabled.
- `CapNativeMove` is **ExtensionOnly** (footnote 12): no native rename; default is Copy+Delete.

Summary: **8 Supported + 3 Conditional + 1 ExtensionOnly + 0 Unsupported**. Full breakdown in [`docs/provider_matrix.md`](../../docs/provider_matrix.md).

## Multipart mapping notes

Azure does **not** have S3-style multipart upload. The driver maps `MultipartService` onto **Block Blob staging**:

- `Initiate` assigns an upload session ID; `UploadPart` calls `StageBlock` with a base64-encoded block ID.
- `Complete` calls `CommitBlockList` (`PutBlockList`).
- `Abort` discards the in-process session; uncommitted blocks are auto-expired by Azure after 7 days.
- `List` returns in-process sessions only — Azure has no server-side listing of uncommitted blocks across blobs.

Key constraints: **minimum staged-block size is 4 MiB** (vs S3's 5 MiB); maximum block count is 50,000.

## Testing

```bash
# Default unit tests (always passes — no Docker needed):
cd providers/azure && go test -short -race -count=1 ./...

# Contract suite (spawns MinIO for wiring smoke test; RunSuite SKIPs — Azure SharedKey/SAS != MinIO SigV4):
go test -tags=docker -count=1 ./...

# Cloud-nightly contract against real Azure Blob Storage:
export OMC_AZURE_NIGHTLY_ACCOUNT=mystorageaccount
export OMC_AZURE_NIGHTLY_KEY=base64encodedkey==
export OMC_AZURE_NIGHTLY_CONTAINER=uos-contract-test
go test -tags=docker -count=1 ./...
```

## See also

- [`docs/provider_matrix.md`](../../docs/provider_matrix.md) — capability matrix
- [`docs/provider_roadmap.md`](../../docs/provider_roadmap.md) — milestone + per-milestone Lessons
- [`CHANGELOG.md`](../../CHANGELOG.md) — full per-module release log
- [`examples/quickstart/`](../../examples/quickstart/) — runnable end-to-end demo
