// Package kms wraps and unwraps the store's data keys under a key-encryption key that stays inside a
// key management service: AWS KMS, Google Cloud KMS, or HashiCorp Vault's transit engine.
//
// It is the half of envelope encryption the seal package does not do. seal seals every value under
// a key derived from a 32-byte key it is handed; this package is how that key can be a random DATA
// key, generated once per store, kept in the store only in the form the KMS wrapped it in, and
// unwrapped by the KMS at startup. The key-encryption key never reaches the process, so the
// configuration holds no secret that opens the store, and revoking the service's access to the KMS
// key revokes its access to every sealed row from the next start.
//
// A Wrapper binds every wrapped key to additional data the caller supplies - the data key's own id,
// in practice - so a wrapped key copied onto another ledger row fails to unwrap.
package kms

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"
)

// Providers are the values storage.encryption.kms.provider accepts.
var Providers = []string{"aws", "gcp", "vault"}

// requestTimeout bounds one call to a KMS. Each happens at startup, once per data key, so a KMS that
// does not answer should fail the start rather than hang it.
const requestTimeout = 30 * time.Second

// ErrUnwrap is returned by Unwrap when the KMS refused to unwrap a key, or returned something that
// is not one: the wrong key-encryption key, revoked access, or a wrapped key that was altered.
var ErrUnwrap = errors.New("the KMS did not unwrap the data key")

// Wrapper wraps data keys under one configured key-encryption key, and unwraps keys wrapped under
// any key-encryption key of the same provider that the service can still use.
type Wrapper interface {
	// Provider is the provider's name, one of Providers.
	Provider() string

	// Key is the configured key-encryption key, in the provider's own form.
	Key() string

	// Wrap encrypts a data key under Key, bound to aad.
	Wrap(ctx context.Context, dataKey []byte, aad []byte) ([]byte, error)

	// Unwrap decrypts a data key that was wrapped under key (which need not be Key: a store keeps
	// the data keys of earlier key-encryption keys), bound to aad.
	Unwrap(ctx context.Context, wrapped []byte, key string, aad []byte) ([]byte, error)
}

// Config selects and configures a Wrapper.
type Config struct {
	// Provider is one of Providers.
	Provider string

	// Key names the key-encryption key: an AWS key ARN, alias ARN, alias or key id; a Google Cloud
	// KMS crypto key's resource name (projects/…/locations/…/keyRings/…/cryptoKeys/…); or a Vault
	// transit key as <mount>/<key>, such as transit/hippocampus.
	Key string

	// Endpoint overrides the provider's endpoint. Vault has no default and needs one; for AWS and
	// Google Cloud it is for a private endpoint or a local emulator.
	Endpoint string

	// Token is the credential for Vault, or a Google Cloud access token in place of the application
	// default credentials. AWS takes its credentials from the SDK's default chain and ignores it.
	Token string

	// HTTPClient is the client the REST providers use. Nil means one with requestTimeout.
	HTTPClient *http.Client
}

// Problems checks a Config without contacting the KMS, for --check-config.
func (c Config) Problems() []error {
	var problems []error

	if !slices.Contains(Providers, c.Provider) {
		problems = append(problems, fmt.Errorf("storage.encryption.kms.provider '%s' is not one of %s",
			c.Provider,
			strings.Join(Providers, ", "),
		))

		return problems
	}

	if c.Key == "" {
		problems = append(problems, fmt.Errorf("storage.encryption.kms.provider '%s' needs storage.encryption.kms.keyId", c.Provider))
	}

	switch c.Provider {

	case "vault":
		if c.Endpoint == "" {
			problems = append(problems, errors.New("storage.encryption.kms.provider 'vault' needs storage.encryption.kms.endpoint, the Vault address"))
		}

		if c.Token == "" {
			problems = append(problems, errors.New("storage.encryption.kms.provider 'vault' needs storage.encryption.kms.token"))
		}

		if c.Key != "" {
			if _, _, err := vaultKeyPath(c.Key); err != nil {
				problems = append(problems, err)
			}
		}

	case "gcp":
		if c.Key != "" && !strings.HasPrefix(c.Key, "projects/") {
			problems = append(problems, fmt.Errorf("storage.encryption.kms.keyId '%s' is not a Google Cloud KMS key name (projects/…/cryptoKeys/…)", c.Key))
		}

	}

	return problems
}

// New builds the Wrapper a Config selects. It does not contact the KMS; the first Wrap or Unwrap
// does.
func New(ctx context.Context, cfg Config) (Wrapper, error) {
	if problems := cfg.Problems(); len(problems) > 0 {
		return nil, errors.Join(problems...)
	}

	client := cfg.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: requestTimeout}
	}

	switch cfg.Provider {

	case "aws":
		return newAWS(ctx, cfg)

	case "gcp":
		return newGCP(ctx, cfg, client)

	default:
		return newVault(cfg, client), nil

	}
}
