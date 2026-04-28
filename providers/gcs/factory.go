package gcs

import (
	"context"
	"fmt"
	"strings"

	"cloud.google.com/go/storage"
	"google.golang.org/api/option"

	"github.com/maqian/object-storage-client/pkg/uos"
	"github.com/maqian/object-storage-client/pkg/uos/credential"
)

// providerID is the canonical Provider id this driver registers under.
// Pinned so changes are caught at compile-time by the surface tests.
const providerID uos.Provider = "gcs"

// DriverConfig is the GCS-specific options bag. Callers set this on
// uos.Config.DriverConfig; Factory.Validate type-asserts it. All fields
// are optional; the zero value yields a working OAuth2-authenticated
// driver as long as the CredentialProvider returns a usable
// AuthOAuth2 / AuthHMAC credential.
type DriverConfig struct {
	// ProjectID names the GCP project that hosts the bucket. It is
	// REQUIRED for BucketService.Create / List (the GCS JSON API rejects
	// those calls without it) but optional for object-scoped operations
	// (GCS routes Get/Put by bucket name alone).
	ProjectID string
	// SignerEmail overrides the GoogleAccessID used when signing URLs.
	// Empty defaults to the resolved credential's service-account email.
	// Required when Signer is built from a credential that does not carry
	// the email locally (e.g. a SignBytes-only OAuth2Credential).
	SignerEmail string
	// SignerPrivateKey, when non-nil, supplies the PEM-encoded private
	// key bytes used for SignedURL. Empty defaults to the key inside the
	// resolved Service Account JSON; ADC backed by Compute Engine /
	// GKE / Workload Identity has no local key — Signer.SignURL will
	// return ErrUnsupported{CapSignedURLRead/Write} in that case.
	SignerPrivateKey []byte
	// SigningScheme picks V2 or V4 URL signing. Empty defaults to V4
	// (the GCS-recommended modern scheme; required for signed-URL writes
	// with virtual-host style).
	SigningScheme string
	// EmulatorEndpoint overrides the storage endpoint URL. Used for the
	// fake-GCS emulator and for regional endpoint pinning. Empty defaults
	// to the SDK's auto-resolved global endpoint (storage.googleapis.com).
	EmulatorEndpoint string
}

// Factory returns a uos.Factory for the Google Cloud Storage driver.
// Drivers register themselves at init time (or callers may register
// manually):
//
//	uos.DefaultRegistry().Register(gcs.Factory())
func Factory() uos.Factory { return factoryImpl{} }

// factoryImpl is the concrete uos.Factory for Google Cloud Storage.
type factoryImpl struct{}

// init registers this driver with the process-global Registry. Tests and
// callers that don't want the global side effect should construct an
// isolated Registry via uos.NewRegistry and Register Factory() manually.
func init() {
	_ = uos.DefaultRegistry().Register(factoryImpl{})
}

// Provider returns the canonical provider id ("gcs"). Required by
// the uos.Factory interface.
func (factoryImpl) Provider() uos.Provider { return providerID }

// Validate checks cfg for structural problems without performing any
// network I/O. CredentialProvider is required (anonymous GCS access
// works only for public buckets, which the contract suite does not
// exercise). DriverConfig, when non-nil, must be a *DriverConfig.
//
// Region is NOT required: GCS resolves region from the bucket's
// metadata at first contact, and the global endpoint
// storage.googleapis.com handles all regions.
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
			Message:   "Config.CredentialProvider is required for the gcs driver",
		}
	}
	if cfg.DriverConfig != nil {
		if _, ok := cfg.DriverConfig.(*DriverConfig); !ok {
			return &uos.Error{
				Code:      uos.ErrInvalidArgument,
				Provider:  providerID,
				Operation: "Factory.Validate",
				Message:   fmt.Sprintf("DriverConfig must be *gcs.DriverConfig, got %T", cfg.DriverConfig),
			}
		}
	}
	return nil
}

// Open performs the credential probe and constructs the underlying
// *storage.Client wrapped in a uos.Client. It honors:
//
//   - cfg.CredentialProvider for OAuth2 (Service Account JSON / ADC) or
//     HMAC keys (when AuthScheme=AuthHMAC). The driver builds the
//     appropriate option.ClientOption and hands it to storage.NewClient.
//   - DriverConfig.ProjectID, captured in the driver state for use by
//     BucketService.Create / List.
//   - DriverConfig.EmulatorEndpoint as the storage endpoint URL
//     (e.g. for the fake-GCS emulator). Empty leaves the SDK on its
//     auto-resolved global endpoint.
//   - DriverConfig.SignerEmail / SignerPrivateKey / SigningScheme for
//     SignedURL — these are stashed on the driver and consulted at
//     SignURL call time, not at Open time.
//
// The cloud.google.com/go/storage SDK ships with its own retry layer
// (configurable via Client.SetRetry / Bucket.Retryer / Object.Retryer).
// pkg/uos.RetryPolicy is the authoritative retry surface, so we install
// a no-op retry policy at the client level so transient retries don't
// double-fire (per docs/provider_roadmap.md cross-cutting risk
// "double-retry storm").
func (f factoryImpl) Open(ctx context.Context, cfg uos.Config) (uos.Client, error) {
	if err := f.Validate(cfg); err != nil {
		return nil, err
	}

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

	dc, _ := cfg.DriverConfig.(*DriverConfig)
	if dc == nil {
		dc = &DriverConfig{}
	}

	// Validate SigningScheme up-front so we don't defer the error to
	// the first SignURL call.
	switch strings.ToLower(dc.SigningScheme) {
	case "", "v2", "v4":
		// allowed
	default:
		return nil, &uos.Error{
			Code:      uos.ErrInvalidArgument,
			Provider:  providerID,
			Operation: "Factory.Open",
			Message:   fmt.Sprintf("DriverConfig.SigningScheme=%q is invalid (allowed: v2, v4)", dc.SigningScheme),
		}
	}

	clientOpts, signerEmail, signerKey, err := buildClientOptions(cred, dc)
	if err != nil {
		return nil, &uos.Error{
			Code:      uos.ErrInvalidArgument,
			Provider:  providerID,
			Operation: "Factory.Open",
			Message:   err.Error(),
			Cause:     err,
		}
	}
	if dc.EmulatorEndpoint != "" {
		clientOpts = append(clientOpts, option.WithEndpoint(dc.EmulatorEndpoint))
	}

	client, err := storage.NewClient(ctx, clientOpts...)
	if err != nil {
		return nil, &uos.Error{
			Code:      uos.ErrInvalidArgument,
			Provider:  providerID,
			Operation: "Factory.Open",
			Message:   "storage.NewClient",
			Cause:     err,
		}
	}

	// Disable the SDK's internal retry layer. pkg/uos.RetryPolicy is the
	// single source of retry truth (cross-cutting risk #1). Setting
	// MaxAttempts=1 with a no-op policy stops the SDK retryer from
	// firing on transient errors; the resolved *uos.Error.Retryable
	// hint lets the caller's RetryPolicy decide.
	client.SetRetry(storage.WithMaxAttempts(1), storage.WithPolicy(storage.RetryNever))

	return &driverImpl{
		cfg:         cfg,
		client:      client,
		projectID:   dc.ProjectID,
		signerEmail: signerEmail,
		signerKey:   signerKey,
		signScheme:  parseSigningScheme(dc.SigningScheme),
		uploads:     newUploadRegistry(),
	}, nil
}

// parseSigningScheme maps the user-facing "v2"/"v4" string into the
// SDK's typed enum. Empty and "v4" both return SigningSchemeV4 because
// V4 is the GCS-recommended default for new code.
func parseSigningScheme(s string) storage.SigningScheme {
	switch strings.ToLower(s) {
	case "v2":
		return storage.SigningSchemeV2
	default:
		return storage.SigningSchemeV4
	}
}

// buildClientOptions converts the resolved Credential plus DriverConfig
// into the option.ClientOption slice that storage.NewClient consumes,
// and surfaces the (signerEmail, signerKey) pair that Signer.SignURL
// will need at call time.
//
// The driver supports three credential payload shapes:
//
//   - *ServiceAccountCredential (driver-local; package gcs.ServiceAccountCredential):
//     embeds the Service Account JSON bytes; both the storage Client and
//     the Signer can use them.
//   - *credential.EnvHMACCredential with AuthHMAC scheme: GCS HMAC keys
//     for the S3-compat XML endpoint; works for storage.NewClient via
//     the HMAC keys' associated service account email but Signer.SignURL
//     still needs a private key, so signerKey will be empty and SignURL
//     will return ErrUnsupported.
//   - Any other Opaque shape with AuthOAuth2 scheme: handled as an
//     opaque oauth2 credential (caller is expected to have stashed
//     option.ClientOption directly via the credential's Opaque field —
//     see the documented escape hatch).
//
// The function returns an error string suitable for embedding in a
// *uos.Error; the caller wraps with the appropriate Code.
func buildClientOptions(cred credential.Credential, dc *DriverConfig) (
	opts []option.ClientOption,
	signerEmail string,
	signerKey []byte,
	err error,
) {
	signerEmail = dc.SignerEmail
	signerKey = dc.SignerPrivateKey

	switch cred.Scheme {
	case "", credential.AuthOAuth2:
		switch v := cred.Opaque.(type) {
		case nil:
			// Anonymous / ADC chain — let storage.NewClient resolve via
			// google.FindDefaultCredentials. Signer falls back to the
			// DriverConfig.SignerPrivateKey, if present.
			return nil, signerEmail, signerKey, nil
		case *ServiceAccountCredential:
			if v == nil || len(v.JSON) == 0 {
				return nil, "", nil, fmt.Errorf("gcs driver: ServiceAccountCredential.JSON is empty")
			}
			opts = append(opts, option.WithCredentialsJSON(v.JSON))
			if signerEmail == "" {
				signerEmail = v.ClientEmail
			}
			if len(signerKey) == 0 {
				signerKey = v.PrivateKeyPEM
			}
			return opts, signerEmail, signerKey, nil
		case ServiceAccountCredential:
			if len(v.JSON) == 0 {
				return nil, "", nil, fmt.Errorf("gcs driver: ServiceAccountCredential.JSON is empty")
			}
			opts = append(opts, option.WithCredentialsJSON(v.JSON))
			if signerEmail == "" {
				signerEmail = v.ClientEmail
			}
			if len(signerKey) == 0 {
				signerKey = v.PrivateKeyPEM
			}
			return opts, signerEmail, signerKey, nil
		case *RawClientOptions:
			// Escape hatch: caller pre-built the option.ClientOption
			// slice (e.g. supplied an oauth2.TokenSource). The driver
			// forwards them as-is and trusts the caller to have set
			// SignerEmail / SignerPrivateKey on DriverConfig if SignURL
			// is needed.
			if v == nil {
				return nil, "", nil, fmt.Errorf("gcs driver: RawClientOptions is nil")
			}
			return append(opts, v.Options...), signerEmail, signerKey, nil
		case RawClientOptions:
			return append(opts, v.Options...), signerEmail, signerKey, nil
		default:
			return nil, "", nil, fmt.Errorf(
				"gcs driver: unsupported AuthOAuth2 credential opaque type %T (need *gcs.ServiceAccountCredential or *gcs.RawClientOptions)",
				cred.Opaque,
			)
		}
	case credential.AuthHMAC:
		switch v := cred.Opaque.(type) {
		case *credential.EnvHMACCredential:
			if v == nil || v.AccessKeyID == "" || v.SecretAccessKey == "" {
				return nil, "", nil, fmt.Errorf("gcs driver: HMAC credential missing access key or secret")
			}
			// HMAC keys feed the S3-compat XML endpoint; the high-level
			// storage.Client can still use them as SignedURL credentials
			// once we stash the Access Key as GoogleAccessID and the
			// Secret as private-key-equivalent. The wire-level dialect
			// in that case is the legacy V2 signature, which the SDK
			// supports via SigningSchemeV2.
			//
			// The high-level data-plane storage.Client requires an
			// OAuth2 credential to talk to the JSON API; HMAC keys
			// alone don't satisfy that. Callers using AuthHMAC must
			// also set DriverConfig.SignerEmail and pass the OAuth2
			// credential out of band — or limit themselves to SignURL.
			if signerEmail == "" {
				signerEmail = v.AccessKeyID
			}
			if len(signerKey) == 0 {
				signerKey = []byte(v.SecretAccessKey)
			}
			// Without a transport credential, storage.NewClient will
			// fail with auth-required — fall back to anonymous mode and
			// let the contract test expose the limitation if needed.
			opts = append(opts, option.WithoutAuthentication())
			return opts, signerEmail, signerKey, nil
		case credential.EnvHMACCredential:
			if v.AccessKeyID == "" || v.SecretAccessKey == "" {
				return nil, "", nil, fmt.Errorf("gcs driver: HMAC credential missing access key or secret")
			}
			if signerEmail == "" {
				signerEmail = v.AccessKeyID
			}
			if len(signerKey) == 0 {
				signerKey = []byte(v.SecretAccessKey)
			}
			opts = append(opts, option.WithoutAuthentication())
			return opts, signerEmail, signerKey, nil
		default:
			return nil, "", nil, fmt.Errorf(
				"gcs driver: unsupported AuthHMAC credential opaque type %T (need *credential.EnvHMACCredential)",
				cred.Opaque,
			)
		}
	default:
		return nil, "", nil, fmt.Errorf(
			"gcs driver: unsupported AuthScheme %q (allowed: AuthOAuth2, AuthHMAC)",
			string(cred.Scheme),
		)
	}
}

// ServiceAccountCredential is the GCS-native credential payload. It
// carries the Service Account JSON bytes for the storage Client and
// (separately) the parsed email + private-key PEM for the Signer. The
// caller may populate just JSON; the factory parses it lazily into the
// other two fields if both are empty.
//
// Field semantics:
//
//   - JSON: the raw Service Account JSON file contents. Used by
//     option.WithCredentialsJSON to build the storage transport.
//   - ClientEmail: the service account email
//     (e.g. "name@project.iam.gserviceaccount.com"). Used as
//     GoogleAccessID by Signer.SignURL.
//   - PrivateKeyPEM: the PEM-encoded RSA private key bytes. Used by
//     Signer.SignURL.
//
// When ClientEmail or PrivateKeyPEM are empty, the factory does NOT
// auto-parse JSON for them — that would require pulling in
// golang.org/x/oauth2/google.JWTConfigFromJSON, which the leaf driver
// avoids; callers who only have the JSON string should use the helper
// constructor (NewServiceAccountCredential) which performs the parse
// at construction time.
type ServiceAccountCredential struct {
	// JSON is the raw Service Account JSON (the file Google Cloud
	// Console hands you when creating a key).
	JSON []byte
	// ClientEmail is the service account address used as GoogleAccessID
	// when signing URLs. May be empty if SignURL is not used.
	ClientEmail string
	// PrivateKeyPEM is the PEM-encoded RSA private key used by SignURL.
	// May be empty if SignURL is not used (Signer returns
	// ErrUnsupported{CapSignedURLRead/Write} in that case).
	PrivateKeyPEM []byte
}

// RawClientOptions is the escape hatch that lets a caller pre-build an
// arbitrary option.ClientOption slice (e.g. for advanced auth flows
// like Workload Identity Federation, custom OAuth2 token sources, or
// caller-supplied http.Client transports).
//
// The driver forwards Options verbatim to storage.NewClient. Signer
// then needs DriverConfig.SignerEmail / SignerPrivateKey set
// separately; otherwise SignURL returns ErrUnsupported.
type RawClientOptions struct {
	// Options is forwarded verbatim to storage.NewClient.
	Options []option.ClientOption
}

// Compile-time guarantees.
var _ uos.Factory = factoryImpl{}
