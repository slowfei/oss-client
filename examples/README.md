# examples

Runnable demos of the unified `pkg/uos` API.

| Example | Module path | What it shows |
| --- | --- | --- |
| [`quickstart/`](quickstart/) | `github.com/maqian/object-storage-client/examples/quickstart` | End-to-end Open + Put + Get + Sign + Delete against any S3-compatible endpoint (defaults to local MinIO). |

Future examples (M6 phase 2/3):

- `multipart/` — `MultipartService.Initiate / UploadPart / Complete / Abort`.
- `direct_grant_qiniu/` — Qiniu Upload Token via `IssueDirectGrant(Mode=Token)`.
- `direct_grant_upyun/` — Upyun FORM upload via `IssueDirectGrant(Mode=Form)`.
- `migration/` — same business code switched across 3 providers via config-only changes.

## Module structure

Each example is its own Go module so it can vendor its own provider-driver
dependencies without dragging them into the root module's transitive set.
The workspace `go.work` file resolves cross-module deps locally for
in-repo development; downstream consumers `go get` the example module
and resolve via the proxy.

To run any example from a fresh checkout:

```bash
cd examples/<example-name>
go run .
```

## Adding a new example

1. Create `examples/<name>/` with its own `go.mod`, `main.go`, and `README.md`.
2. Add `./examples/<name>` to the workspace `use (...)` block in `go.work`.
3. Add a versioned `replace` line for the example module's in-repo dependencies
   to the workspace `replace (...)` block in `go.work` (mirror the existing
   provider entries).
4. Run `go build ./examples/<name>` from the repo root to verify it compiles.
5. Add a row to the table above.
