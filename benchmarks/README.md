# object-storage-client benchmarks

Per-provider throughput benchmarks for the unified Go object-storage SDK.
AWS and MinIO are the M6 phase 2 baseline (S3-family); per-vendor sweeps for
the other 8 providers (Alibaba, Azure, GCS, Huawei, Qiniu, Tencent, Upyun,
Volcengine) land in M6 phase 3 / v1.0.0. The benchmark structure is
per-provider so CI can run driver suites in parallel; future drivers paste and
rename the file.

---

## Requirements

- Go 1.25+
- Docker (testcontainers spins up a MinIO container automatically)
- The repo workspace (`go.work`) must include this module; the lead adds the
  `use ./benchmarks` directive and a `replace` block before the M6 merge.

---

## Run

### Full benchmark suite (~5-10 min, Docker required)

```sh
go test -tags=docker -bench=. -benchmem ./benchmarks/...
```

### AWS provider only

```sh
go test -tags=docker -bench=. -benchmem ./benchmarks/aws
```

### MinIO provider only

```sh
go test -tags=docker -bench=. -benchmem ./benchmarks/minio
```

### Signing benchmarks only (no container needed for AWS; AWS SignURL is offline)

```sh
go test -tags=docker -bench=BenchmarkSignURL -benchmem ./benchmarks/aws
```

Note: the MinIO driver's `SignURL` also generates the URL locally (no HTTP
round-trip), so both signing benchmarks are effectively offline once the
container startup is complete.

### Adjust iteration count

```sh
go test -tags=docker -bench=BenchmarkPut_Small -benchtime=10s -benchmem ./benchmarks/aws
```

---

## What is measured

| Benchmark function      | Driver path measured                                          |
|-------------------------|---------------------------------------------------------------|
| `BenchmarkPut_Small`    | `ObjectService.Put` — 4 KiB body, unique key per iteration   |
| `BenchmarkGet_Small`    | `ObjectService.Get` — 4 KiB body, same key every iteration   |
| `BenchmarkPut_Medium_1MiB` | `ObjectService.Put` — 1 MiB body, unique key per iteration |
| `BenchmarkMultipart_15MiB` | `MultipartService.Initiate` + 3×`UploadPart`(5 MiB) + `Complete` per iteration |
| `BenchmarkSignURL_Read` | `Signer.SignURL(GET)` — pure signing CPU, no network I/O; run with `b.RunParallel` |

Each file (`aws_test.go`, `minio_test.go`) runs an independent MinIO container
so the two providers can be benchmarked in isolation or in parallel CI lanes.

---

## Reading results

`go test -bench` produces one line per benchmark function:

```
BenchmarkPut_Small-8     200     5 412 345 ns/op    3.97 MB/s    4096 B/op    12 allocs/op
```

| Column          | Meaning |
|-----------------|---------|
| `200`           | Number of iterations (`b.N`) the framework ran |
| `5 412 345 ns/op` | Wall-clock nanoseconds per iteration |
| `3.97 MB/s`     | Throughput — computed from `b.SetBytes` / ns/op. Only present when `b.SetBytes` was called |
| `4096 B/op`     | Heap bytes allocated per iteration (requires `-benchmem`) |
| `12 allocs/op`  | Number of distinct heap allocations per iteration |

**What is "good":**

- `ns/op` lower is better. For loopback Put/Get expect 2–20 ms (2–20 M ns) per
  operation; the limiting factor is Docker networking, not the SDK itself.
- `MB/s` higher is better. 1 MiB Put over loopback should yield 50–500 MB/s
  depending on host hardware; well below the theoretical RAM throughput because
  HTTP framing + TLS-or-plain-TCP + testcontainers overhead dominate.
- `allocs/op` lower is better. The SDK aims for < 30 allocs per Put/Get at the
  unified-API layer; driver internals add more. High allocations hurt GC pause
  under sustained load.
- `B/op` at 4 KiB Put should be close to 4096 plus a small fixed overhead
  (< 10 KiB). A value much larger than the payload indicates unnecessary
  buffering.

---

## Comparison guidance

### AWS vs MinIO baseline expectations

Both providers use the SigV4 signing family but different SDK layers:

- **AWS driver** (`providers/aws`): built on `aws-sdk-go-v2/service/s3` with
  internal retry disabled. It carries aws-sdk-go-v2 middleware overhead
  (endpoint resolution, checksum calculation, request signing).
- **MinIO driver** (`providers/minio`): built on `minio-go/v7` with a simpler
  request pipeline. In micro-benchmarks against the same testcontainers MinIO
  instance, the MinIO driver typically shows 10–30% lower ns/op on Put and Get
  because its request-construction path is shorter.

In both cases the **dominant cost on testcontainers is Docker loopback
latency** (1–5 ms per RTT), not SDK overhead. Do not draw production
conclusions from testcontainers numbers alone.

### Real-cloud comparison

For production-representative numbers, run against real AWS S3 using the
cloud-nightly environment variables:

```sh
export OMC_BENCH_ENDPOINT=   # empty → AWS default
export OMC_BENCH_REGION=us-east-1
export OMC_BENCH_KEY=AKIA...
export OMC_BENCH_SECRET=...
go test -tags=docker -bench=. -benchmem -benchtime=30s ./benchmarks/aws
```

Cloud-nightly results are **not** comparable to testcontainers results: network
latency from a CI runner to S3 is 20–80 ms vs < 1 ms loopback.

---

## Future work

M6 phase 3 / v1.0.0 will add benchmark files for the 8 remaining providers.
The pattern is paste-and-rename:

1. Copy `aws_test.go` (or `minio_test.go`) to `<provider>_test.go`.
2. Replace the `setup<AWS|Minio>` function with a provider-specific open call.
3. Note: GCS, Azure, Qiniu, Upyun use non-MinIO testcontainer fixtures or
   emulators; the `SpawnMinIO` helper is S3-compatible providers only.
4. Add the new module to `go.work use` + `replace` block.
5. Run `go test -tags=docker -bench=. -benchmem ./<provider>` to validate.

---

## Caveats

- **Loopback latency dominates**: testcontainers measurements reflect
  HTTP-over-loopback cost, not actual cloud storage backend cost. They are
  useful for comparing SDK serialisation, signing overhead, and allocation
  profiles between drivers — not for predicting production throughput.

- **Multipart benchmarks are sequential within each iteration**: MinIO in a
  single testcontainers container is a single-machine server. Issuing parallel
  `UploadPart` calls within one iteration would increase concurrency against a
  single local process, not improve the baseline measurement. The benchmark
  therefore uses sequential part uploads to keep ns/op deterministic.

- **`MultipartService.List` benchmark not included for non-S3 drivers**: GCS,
  Azure, Qiniu, Upyun, and others implement `MultipartService.List` as an
  in-process-only stub (per M4/M5 driver lessons). Benchmarking a no-op list
  would produce misleadingly low ns/op that is not representative of the
  S3-family's network-backed list. The list benchmark will be added per-driver
  only when the driver's `List` issues real network calls.

- **Container startup excluded from timing**: each benchmark calls
  `b.ResetTimer()` after `SpawnMinIO` and bucket creation complete, so the
  reported ns/op measures only the operation under test, not Docker pull time.

- **`-race` is safe**: all benchmarks use `context.Background()` per iteration
  and do not share mutable state across goroutines (the `RunParallel` benchmarks
  only share the immutable `env` struct and the key string).
