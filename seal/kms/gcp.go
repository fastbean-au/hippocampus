package kms

import (
	"context"
	"encoding/base64"
	"fmt"
	"net/http"
	"strings"

	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
)

// gcpEndpoint is Cloud KMS's REST endpoint.
const gcpEndpoint = "https://cloudkms.googleapis.com"

// gcpScope is the OAuth2 scope the application default credentials are asked for.
const gcpScope = "https://www.googleapis.com/auth/cloudkms"

// gcpWrapper is Google Cloud KMS, over its REST API rather than the client library, which would
// bring a gRPC stack and a dependency tree larger than the rest of the service's for two calls.
type gcpWrapper struct {
	key      string
	endpoint string
	tokens   oauth2.TokenSource
	client   *http.Client
}

// newGCP builds the Cloud KMS client. Without a configured token it takes the application default
// credentials: GOOGLE_APPLICATION_CREDENTIALS, gcloud's, or the workload's service account.
func newGCP(ctx context.Context, cfg Config, client *http.Client) (*gcpWrapper, error) {
	endpoint := strings.TrimRight(cfg.Endpoint, "/")
	if endpoint == "" {
		endpoint = gcpEndpoint
	}

	var tokens oauth2.TokenSource

	if cfg.Token != "" {
		tokens = oauth2.StaticTokenSource(&oauth2.Token{AccessToken: cfg.Token})
	} else {
		found, err := google.DefaultTokenSource(ctx, gcpScope)
		if err != nil {
			return nil, fmt.Errorf("finding Google Cloud credentials for KMS (set storage.encryption.kms.token, or provide application default credentials): %w", err)
		}

		tokens = found
	}

	return &gcpWrapper{key: cfg.Key, endpoint: endpoint, tokens: tokens, client: client}, nil
}

// Provider implements Wrapper.
func (w *gcpWrapper) Provider() string {
	return "gcp"
}

// Key implements Wrapper.
func (w *gcpWrapper) Key() string {
	return w.key
}

// gcpRequest is the body of an encrypt or decrypt call; each sends the half it needs.
type gcpRequest struct {
	Plaintext                   string `json:"plaintext,omitempty"`
	Ciphertext                  string `json:"ciphertext,omitempty"`
	AdditionalAuthenticatedData string `json:"additionalAuthenticatedData"`
}

// gcpResponse is the body of an encrypt or decrypt response.
type gcpResponse struct {
	Plaintext  string `json:"plaintext"`
	Ciphertext string `json:"ciphertext"`
}

// Wrap implements Wrapper.
func (w *gcpWrapper) Wrap(ctx context.Context, dataKey []byte, aad []byte) ([]byte, error) {
	var out gcpResponse

	in := gcpRequest{
		Plaintext:                   base64.StdEncoding.EncodeToString(dataKey),
		AdditionalAuthenticatedData: base64.StdEncoding.EncodeToString(aad),
	}

	if err := w.call(ctx, w.key, "encrypt", in, &out); err != nil {
		return nil, err
	}

	wrapped, err := base64.StdEncoding.DecodeString(out.Ciphertext)
	if err != nil || len(wrapped) == 0 {
		return nil, fmt.Errorf("gcp KMS encrypt under %s returned no ciphertext", w.key)
	}

	return wrapped, nil
}

// Unwrap implements Wrapper.
func (w *gcpWrapper) Unwrap(ctx context.Context, wrapped []byte, key string, aad []byte) ([]byte, error) {
	var out gcpResponse

	in := gcpRequest{
		Ciphertext:                  base64.StdEncoding.EncodeToString(wrapped),
		AdditionalAuthenticatedData: base64.StdEncoding.EncodeToString(aad),
	}

	if err := w.call(ctx, key, "decrypt", in, &out); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrUnwrap, err)
	}

	dataKey, err := base64.StdEncoding.DecodeString(out.Plaintext)
	if err != nil || len(dataKey) == 0 {
		return nil, fmt.Errorf("gcp KMS decrypt under %s: %w: the reply holds no base64 plaintext", key, ErrUnwrap)
	}

	return dataKey, nil
}

// call makes one encrypt or decrypt call against a key.
func (w *gcpWrapper) call(ctx context.Context, key string, method string, in gcpRequest, out *gcpResponse) error {
	token, err := w.tokens.Token()
	if err != nil {
		return fmt.Errorf("obtaining a Google Cloud access token: %w", err)
	}

	url := w.endpoint + "/v1/" + key + ":" + method

	headers := map[string]string{"Authorization": "Bearer " + token.AccessToken}

	if err := postJSON(ctx, w.client, url, headers, in, out); err != nil {
		return fmt.Errorf("gcp KMS %s under %s: %w", method, key, err)
	}

	return nil
}
