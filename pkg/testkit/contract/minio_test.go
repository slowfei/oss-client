//go:build docker

package contract

import (
	"context"
	"net/http"
	"testing"
	"time"
)

func TestSpawnMinIO_Smoke(t *testing.T) {
	if testing.Short() {
		t.Skip("requires Docker; -short skips testcontainers")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	endpoint, _, _, cleanup, err := SpawnMinIO(ctx)
	if err != nil {
		t.Fatalf("SpawnMinIO: %v", err)
	}
	defer cleanup()
	if endpoint == "" {
		t.Fatal("SpawnMinIO: empty endpoint")
	}
	// MinIO answers /minio/health/live with 200 once it's ready.
	url := "http://" + endpoint + "/minio/health/live"
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("health probe: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("health probe: status %d", resp.StatusCode)
	}
}
