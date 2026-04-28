//go:build docker

// File minio.go is the testcontainers-go-backed MinIO helper used by
// drivers that want a Dockerised reference S3-compatible server during
// contract tests. The build tag ensures the default `go test ./...`
// run does NOT pull testcontainers (and therefore Docker) into scope.
//
// To exercise: `go test ./pkg/testkit/contract/... -tags=docker`.

package contract

import (
	"context"
	"fmt"

	"github.com/testcontainers/testcontainers-go"
	tcminio "github.com/testcontainers/testcontainers-go/modules/minio"
)

// SpawnMinIO starts a MinIO container via testcontainers-go and
// returns its endpoint, root credentials, and a cleanup closure that
// terminates the container. The cleanup MUST be called by the caller
// (typically via defer or t.Cleanup); failing to do so leaks a Docker
// container.
//
// The MinIO image used is the testcontainers-go module default; pin
// callers should switch to tcminio.Run with an explicit image tag.
func SpawnMinIO(ctx context.Context) (endpoint, accessKey, secretKey string, cleanup func(), err error) {
	const (
		defaultImage    = "minio/minio:RELEASE.2024-08-29T01-40-52Z"
		defaultUser     = "uoscontract"
		defaultPassword = "uoscontractkey"
	)
	c, err := tcminio.Run(ctx, defaultImage,
		tcminio.WithUsername(defaultUser),
		tcminio.WithPassword(defaultPassword),
	)
	if err != nil {
		return "", "", "", nil, fmt.Errorf("contract: spawn MinIO: %w", err)
	}
	host, err := c.ConnectionString(ctx)
	if err != nil {
		_ = testcontainers.TerminateContainer(c)
		return "", "", "", nil, fmt.Errorf("contract: MinIO endpoint: %w", err)
	}
	return host, defaultUser, defaultPassword, func() {
		_ = testcontainers.TerminateContainer(c)
	}, nil
}
