//go:build docker

// File gcs_emulator.go provides the in-process fake-gcs-server helper used
// by providers/gcs/driver_test.go to run the contract suite against a real
// GCS JSON API emulator without Docker. The build tag mirrors minio.go so
// the default `go test ./...` run does not pull the fakestorage dependency.
//
// To exercise: `go test ./pkg/testkit/contract/... -tags=docker`.
package contract

import (
	"context"
	"fmt"

	"github.com/fsouza/fake-gcs-server/fakestorage"
)

// SpawnFakeGCS starts an in-process fake-gcs-server backed by an
// httptest.Server and returns:
//   - endpoint: the http://127.0.0.1:NNNN base URL (no trailing path).
//     Pass this as DriverConfig.EmulatorEndpoint; the gcs driver appends
//     the correct /storage/v1/ suffix via option.WithEndpoint.
//   - cleanup: stops the server; the caller MUST call this (typically via
//     defer or t.Cleanup).
//   - err: non-nil if the server failed to start.
//
// The server listens on an ephemeral port (Port: 0), uses plain HTTP, and
// pre-creates no buckets — driver tests create buckets via cli.Buckets().Create.
// No Docker is required; the server runs entirely in-process.
func SpawnFakeGCS(_ context.Context) (endpoint string, cleanup func(), err error) {
	srv, err := fakestorage.NewServerWithOptions(fakestorage.Options{
		// Plain HTTP so the gcs driver can reach it without TLS cert setup.
		Scheme: "http",
		// Host "127.0.0.1" keeps traffic local; port 0 = ephemeral.
		Host: "127.0.0.1",
		Port: 0,
	})
	if err != nil {
		return "", nil, fmt.Errorf("contract: spawn fake-gcs-server: %w", err)
	}
	url := srv.URL()
	if url == "" {
		srv.Stop()
		return "", nil, fmt.Errorf("contract: fake-gcs-server: URL() returned empty string")
	}
	return url, srv.Stop, nil
}
