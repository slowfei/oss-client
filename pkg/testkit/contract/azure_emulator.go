//go:build docker

// File azure_emulator.go provides the Azurite (Microsoft Azure Storage
// emulator) testcontainers helper used by providers/azure/driver_test.go to
// run the contract suite without real Azure credentials. The build tag mirrors
// minio.go so the default `go test ./...` run does not pull Docker into scope.
//
// To exercise: `go test ./pkg/testkit/contract/... -tags=docker`.
package contract

import (
	"context"
	"fmt"

	"github.com/testcontainers/testcontainers-go"
	tcazurite "github.com/testcontainers/testcontainers-go/modules/azure/azurite"
)

// SpawnAzurite starts a Microsoft Azurite container (Azure Storage emulator)
// via testcontainers-go and returns:
//   - serviceURL: the http://127.0.0.1:NNNNN/devstoreaccount1 Blob service
//     base URL. Pass this as DriverConfig.ServiceURL.
//   - accountName: always "devstoreaccount1" (Azurite's hardcoded test account).
//   - accountKey: Azurite's hardcoded test key (publicly documented).
//   - cleanup: terminates the container; the caller MUST call this.
//   - err: non-nil if container startup failed.
//
// Uses the official testcontainers azurite module. The container exposes only
// the Blob service port (10000/tcp); Queue and Table are not started, keeping
// startup time short. Azurite container start typically takes ~10 seconds.
func SpawnAzurite(ctx context.Context) (serviceURL, accountName, accountKey string, cleanup func(), err error) {
	ctr, err := tcazurite.Run(ctx,
		"mcr.microsoft.com/azure-storage/azurite:latest",
		// Enable only the Blob service to minimise startup time.
		tcazurite.WithEnabledServices(tcazurite.BlobService),
		// Skip Azure SDK API version enforcement: the azblob SDK in this
		// repo may send a newer x-ms-version header than the pinned
		// Azurite image supports. --skipApiVersionCheck tells Azurite to
		// accept all API versions, matching real-world emulator usage.
		testcontainers.WithCmdArgs("--skipApiVersionCheck"),
	)
	if err != nil {
		return "", "", "", nil, fmt.Errorf("contract: spawn azurite: %w", err)
	}

	blobURL, err := ctr.BlobServiceURL(ctx)
	if err != nil {
		_ = testcontainers.TerminateContainer(ctr)
		return "", "", "", nil, fmt.Errorf("contract: azurite BlobServiceURL: %w", err)
	}

	// BlobServiceURL returns http://host:port; Azurite expects requests
	// scoped to /devstoreaccount1 (it does not use virtual-host style).
	serviceURL = blobURL + "/" + tcazurite.AccountName

	return serviceURL,
		tcazurite.AccountName,
		tcazurite.AccountKey,
		func() { _ = testcontainers.TerminateContainer(ctr) },
		nil
}
