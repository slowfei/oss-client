// Package azure is the native uos.Client driver for Azure Blob Storage.
// It targets the v0.1 frozen pkg/uos surface (architecture_plan §1) and
// implements every method on uos.Client by translating to/from
// github.com/Azure/azure-sdk-for-go/sdk/storage/azblob.
//
// # Bucket → Container mapping
//
// Azure Blob Storage organises objects under Containers, not Buckets.
// The driver maps the unified Bucket concept 1:1 onto Azure Containers.
// The Storage Account is a driver-level concept (DriverConfig.StorageAccount)
// because it encodes the geographic location — unlike S3 where region is
// a separate Config.Region field. There is therefore no Config.Region
// for azure; callers must set DriverConfig.StorageAccount.
//
// # Auth shapes
//
// Three auth shapes are supported via CredentialProvider:
//   - AuthSharedKey  — AccountName + AccountKey → azblob.SharedKeyCredential.
//   - AuthSAS        — pre-formed SAS token string → NoCredential + SAS appended to URL.
//   - AuthCustom     — Entra ID / user-delegation → azidentity.DefaultAzureCredential
//     or a caller-supplied azcore.TokenCredential stored in Opaque.
//
// # SAS start-time semantics
//
// Azure SAS tokens carry an optional start time (signedstart=). The unified
// SignURLRequest.ExpiresIn does not expose a start-time offset. The driver
// sets start = now−5 min for clock-skew tolerance; expiry = now+ExpiresIn.
// See signerService.SignURL doc comment and Lessons (M4) in provider_roadmap.md.
//
// # Block Blob multipart
//
// Azure does not have S3-style multipart upload. Block Blob staging maps
// onto MultipartService: Initiate allocates an upload session; UploadPart
// stages one block (base64-encoded ID); Complete calls PutBlockList.
// Minimum staged-block size is 4 MiB (vs S3's 5 MiB); maximum block count
// is 50,000. See driver.go multipartService and Lessons (M4).
package azure

import (
	"context"
	"fmt"
	"strings"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob"

	"github.com/maqian/object-storage-client/pkg/uos"
	"github.com/maqian/object-storage-client/pkg/uos/credential"
)

// providerID is the canonical Provider id this driver registers under.
const providerID uos.Provider = "azure"

// DriverConfig is the Azure-specific options bag. Callers set this on
// uos.Config.DriverConfig; Factory.Validate type-asserts it.
//
// StorageAccount is mandatory: Azure Blob Storage endpoints are per-account
// (e.g. https://<account>.blob.core.windows.net), so unlike S3-family
// providers there is no Config.Region field — the storage account encodes
// the geographic location and forms the base URL.
type DriverConfig struct {
	// StorageAccount is the Azure Storage Account name. Required.
	// Used to construct the service URL:
	//   https://<StorageAccount>.blob.core.windows.net/
	StorageAccount string

	// ServiceURL overrides the auto-derived service URL. Optional; useful
	// for Azurite (the local Azure Blob Storage emulator) or sovereign clouds
	// (e.g. "https://<account>.blob.core.chinacloudapi.cn/").
	// When empty, the standard URL is derived from StorageAccount.
	ServiceURL string

	// APIVersion pins the x-ms-version header sent on every request.
	// Empty uses the azblob SDK default (currently "2024-11-04").
	APIVersion string
}

// SharedKeyCredential is the concrete Opaque payload for AuthSharedKey.
// Callers supply it via credential.NewStatic(credential.Credential{
//
//	Scheme: credential.AuthSharedKey,
//	Opaque: &azure.SharedKeyCredential{AccountName: "…", AccountKey: "…"},
//
// }).
type SharedKeyCredential struct {
	// AccountName is the Storage Account name.
	AccountName string
	// AccountKey is the base64-encoded account key (the "key1" or "key2"
	// value from the Azure portal / az storage account keys list).
	AccountKey string
}

// SASCredential is the concrete Opaque payload for AuthSAS.
// Token is a pre-formed SAS query string (starting with "?" or without it).
type SASCredential struct {
	// Token is the SAS query string, e.g. "sv=2022-11-02&ss=b&…".
	// Leading "?" is stripped automatically.
	Token string
}

// TokenCredential is the concrete Opaque payload for AuthCustom when the
// caller supplies a pre-built azcore.TokenCredential (e.g. from azidentity).
// If Opaque is nil or is not a TokenCredential, the driver falls back to
// azidentity.NewDefaultAzureCredential (ADC chain).
type TokenCredential struct {
	// Credential is the azcore.TokenCredential to use (e.g. a
	// *azidentity.ClientSecretCredential for service-principal auth).
	Credential azcore.TokenCredential
}

// Factory returns a uos.Factory for the Azure Blob Storage driver.
func Factory() uos.Factory { return factoryImpl{} }

// factoryImpl is the concrete uos.Factory for Azure Blob Storage.
type factoryImpl struct{}

// init registers this driver with the process-global Registry.
func init() {
	_ = uos.DefaultRegistry().Register(factoryImpl{})
}

// Provider returns the canonical provider id ("azure").
func (factoryImpl) Provider() uos.Provider { return providerID }

// Validate checks cfg for structural problems without performing any
// network I/O. DriverConfig.StorageAccount is required (Azure has no
// S3-style region — the storage account encodes the location).
// CredentialProvider is required. DriverConfig, when non-nil, must be
// a *DriverConfig.
func (factoryImpl) Validate(cfg uos.Config) error {
	if cfg.Provider != "" && cfg.Provider != providerID {
		return &uos.Error{
			Code:      uos.ErrInvalidArgument,
			Provider:  providerID,
			Operation: "Factory.Validate",
			Message: fmt.Sprintf(
				"Config.Provider=%q does not match this Factory (%q)",
				string(cfg.Provider), string(providerID),
			),
		}
	}
	if cfg.CredentialProvider == nil {
		return &uos.Error{
			Code:      uos.ErrInvalidArgument,
			Provider:  providerID,
			Operation: "Factory.Validate",
			Message:   "Config.CredentialProvider is required for the azure driver",
		}
	}
	if cfg.DriverConfig == nil {
		return &uos.Error{
			Code:      uos.ErrInvalidArgument,
			Provider:  providerID,
			Operation: "Factory.Validate",
			Message:   "Config.DriverConfig (*azure.DriverConfig) is required for the azure driver",
		}
	}
	dc, ok := cfg.DriverConfig.(*DriverConfig)
	if !ok {
		return &uos.Error{
			Code:      uos.ErrInvalidArgument,
			Provider:  providerID,
			Operation: "Factory.Validate",
			Message:   fmt.Sprintf("DriverConfig must be *azure.DriverConfig, got %T", cfg.DriverConfig),
		}
	}
	if strings.TrimSpace(dc.StorageAccount) == "" && strings.TrimSpace(dc.ServiceURL) == "" {
		return &uos.Error{
			Code:      uos.ErrInvalidArgument,
			Provider:  providerID,
			Operation: "Factory.Validate",
			Message:   "DriverConfig.StorageAccount is required for the azure driver (it encodes the storage location)",
		}
	}
	return nil
}

// Open resolves the credential and constructs the azblob.Client. Three
// auth paths are supported:
//
//   - AuthSharedKey  → azblob.NewClientWithSharedKeyCredential
//   - AuthSAS        → azblob.NewClientWithNoCredential (SAS appended to URL)
//   - AuthCustom     → azblob.NewClient with azcore.TokenCredential
//     (azidentity.DefaultAzureCredential when Opaque is nil or not a
//     *TokenCredential).
//
// The Azure SDK ships with an internal retryer (policy.RetryOptions).
// It is disabled here (MaxRetries=0) so pkg/uos.RetryPolicy is the
// sole retry surface — per docs/provider_roadmap.md cross-cutting risk
// "double-retry storm".
func (f factoryImpl) Open(ctx context.Context, cfg uos.Config) (uos.Client, error) {
	if err := f.Validate(cfg); err != nil {
		return nil, err
	}
	dc := cfg.DriverConfig.(*DriverConfig)

	cred, err := cfg.CredentialProvider.Resolve(ctx, string(providerID))
	if err != nil {
		return nil, &uos.Error{
			Code:      uos.ErrUnauthenticated,
			Provider:  providerID,
			Operation: "Factory.Open",
			Message:   "credential provider failed",
			Cause:     err,
		}
	}

	serviceURL := dc.ServiceURL
	if serviceURL == "" {
		serviceURL = fmt.Sprintf("https://%s.blob.core.windows.net/", dc.StorageAccount)
	}
	// Ensure trailing slash so URL joins work cleanly.
	if !strings.HasSuffix(serviceURL, "/") {
		serviceURL += "/"
	}

	clientOpts := &azblob.ClientOptions{}
	// Disable the SDK's internal retryer — pkg/uos.RetryPolicy is the
	// single source of retry truth (cross-cutting risk: "double-retry storm").
	clientOpts.Retry.MaxRetries = 0

	var client *azblob.Client
	switch cred.Scheme {
	case credential.AuthSharedKey:
		skc, err := extractSharedKey(cred)
		if err != nil {
			return nil, &uos.Error{
				Code:      uos.ErrInvalidArgument,
				Provider:  providerID,
				Operation: "Factory.Open",
				Message:   err.Error(),
				Cause:     err,
			}
		}
		sharedKey, err := azblob.NewSharedKeyCredential(skc.AccountName, skc.AccountKey)
		if err != nil {
			return nil, &uos.Error{
				Code:      uos.ErrUnauthenticated,
				Provider:  providerID,
				Operation: "Factory.Open",
				Message:   "azblob.NewSharedKeyCredential failed",
				Cause:     err,
			}
		}
		client, err = azblob.NewClientWithSharedKeyCredential(serviceURL, sharedKey, clientOpts)
		if err != nil {
			return nil, &uos.Error{
				Code:      uos.ErrInvalidArgument,
				Provider:  providerID,
				Operation: "Factory.Open",
				Message:   "azblob.NewClientWithSharedKeyCredential",
				Cause:     err,
			}
		}
		return &driverImpl{
			cfg:        cfg,
			dc:         dc,
			client:     client,
			authScheme: credential.AuthSharedKey,
			sharedKey:  sharedKey,
		}, nil

	case credential.AuthSAS:
		sasc, err := extractSAS(cred)
		if err != nil {
			return nil, &uos.Error{
				Code:      uos.ErrInvalidArgument,
				Provider:  providerID,
				Operation: "Factory.Open",
				Message:   err.Error(),
				Cause:     err,
			}
		}
		sasToken := strings.TrimPrefix(sasc.Token, "?")
		// Append SAS token to the service URL so every request carries it.
		sasURL := serviceURL
		if strings.Contains(sasURL, "?") {
			sasURL += "&" + sasToken
		} else {
			sasURL += "?" + sasToken
		}
		client, err = azblob.NewClientWithNoCredential(sasURL, clientOpts)
		if err != nil {
			return nil, &uos.Error{
				Code:      uos.ErrInvalidArgument,
				Provider:  providerID,
				Operation: "Factory.Open",
				Message:   "azblob.NewClientWithNoCredential (SAS)",
				Cause:     err,
			}
		}
		return &driverImpl{
			cfg:        cfg,
			dc:         dc,
			client:     client,
			authScheme: credential.AuthSAS,
		}, nil

	case credential.AuthCustom:
		tokenCred, err := extractTokenCredential(cred)
		if err != nil {
			return nil, &uos.Error{
				Code:      uos.ErrInvalidArgument,
				Provider:  providerID,
				Operation: "Factory.Open",
				Message:   err.Error(),
				Cause:     err,
			}
		}
		client, err = azblob.NewClient(serviceURL, tokenCred, clientOpts)
		if err != nil {
			return nil, &uos.Error{
				Code:      uos.ErrInvalidArgument,
				Provider:  providerID,
				Operation: "Factory.Open",
				Message:   "azblob.NewClient (Entra ID / token credential)",
				Cause:     err,
			}
		}
		return &driverImpl{
			cfg:        cfg,
			dc:         dc,
			client:     client,
			authScheme: credential.AuthCustom,
			tokenCred:  tokenCred,
		}, nil

	default:
		return nil, &uos.Error{
			Code:      uos.ErrInvalidArgument,
			Provider:  providerID,
			Operation: "Factory.Open",
			Message:   fmt.Sprintf("azure driver requires AuthSharedKey, AuthSAS, or AuthCustom, got %q", string(cred.Scheme)),
		}
	}
}

// extractSharedKey unwraps a credential carrying a *SharedKeyCredential payload.
func extractSharedKey(c credential.Credential) (*SharedKeyCredential, error) {
	switch v := c.Opaque.(type) {
	case *SharedKeyCredential:
		if v == nil || v.AccountName == "" || v.AccountKey == "" {
			return nil, fmt.Errorf("azure driver: SharedKeyCredential missing AccountName or AccountKey")
		}
		return v, nil
	case SharedKeyCredential:
		if v.AccountName == "" || v.AccountKey == "" {
			return nil, fmt.Errorf("azure driver: SharedKeyCredential missing AccountName or AccountKey")
		}
		return &v, nil
	default:
		return nil, fmt.Errorf(
			"azure driver: AuthSharedKey requires *azure.SharedKeyCredential opaque payload, got %T",
			c.Opaque,
		)
	}
}

// extractSAS unwraps a credential carrying a *SASCredential payload.
func extractSAS(c credential.Credential) (*SASCredential, error) {
	switch v := c.Opaque.(type) {
	case *SASCredential:
		if v == nil || v.Token == "" {
			return nil, fmt.Errorf("azure driver: SASCredential missing Token")
		}
		return v, nil
	case SASCredential:
		if v.Token == "" {
			return nil, fmt.Errorf("azure driver: SASCredential missing Token")
		}
		return &v, nil
	default:
		return nil, fmt.Errorf(
			"azure driver: AuthSAS requires *azure.SASCredential opaque payload, got %T",
			c.Opaque,
		)
	}
}

// extractTokenCredential unwraps a credential carrying an azcore.TokenCredential
// or falls back to azidentity.NewDefaultAzureCredential (ADC chain).
func extractTokenCredential(c credential.Credential) (azcore.TokenCredential, error) {
	switch v := c.Opaque.(type) {
	case *TokenCredential:
		if v != nil && v.Credential != nil {
			return v.Credential, nil
		}
	case TokenCredential:
		if v.Credential != nil {
			return v.Credential, nil
		}
	case azcore.TokenCredential:
		if v != nil {
			return v, nil
		}
	}
	// Fall back to the Azure Default Credential chain (env vars, workload
	// identity, managed identity, Azure CLI, etc.).
	adc, err := azidentity.NewDefaultAzureCredential(nil)
	if err != nil {
		return nil, fmt.Errorf("azure driver: azidentity.NewDefaultAzureCredential: %w", err)
	}
	return adc, nil
}

// Compile-time guarantees.
var _ uos.Factory = factoryImpl{}
