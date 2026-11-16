package kms

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// maxResponseBytes bounds a KMS response body. A wrapped 32-byte key and its envelope come to a few
// hundred bytes; anything near this is not a KMS answering.
const maxResponseBytes = 1 << 20

// vaultWrapper is Vault's transit secrets engine.
type vaultWrapper struct {
	key      string
	endpoint string
	token    string
	client   *http.Client
}

// newVault builds the transit client. Problems has already checked the key and endpoint.
func newVault(cfg Config, client *http.Client) *vaultWrapper {
	return &vaultWrapper{
		key:      cfg.Key,
		endpoint: strings.TrimRight(cfg.Endpoint, "/"),
		token:    cfg.Token,
		client:   client,
	}
}

// vaultKeyPath splits a transit key reference, <mount>/<key>, at its last slash. The mount may hold
// slashes of its own - a namespaced or nested mount - and a key name cannot.
func vaultKeyPath(key string) (string, string, error) {
	i := strings.LastIndex(key, "/")

	if i <= 0 || i == len(key)-1 {
		return "", "", fmt.Errorf("storage.encryption.kms.keyId '%s' is not a Vault transit key as <mount>/<key>, such as transit/hippocampus", key)
	}

	return key[:i], key[i+1:], nil
}

// Provider implements Wrapper.
func (w *vaultWrapper) Provider() string {
	return "vault"
}

// Key implements Wrapper.
func (w *vaultWrapper) Key() string {
	return w.key
}

// vaultRequest is the body of an encrypt or decrypt call. associated_data needs an AEAD key type,
// which every transit default (aes256-gcm96) is.
type vaultRequest struct {
	Plaintext      string `json:"plaintext,omitempty"`
	Ciphertext     string `json:"ciphertext,omitempty"`
	AssociatedData string `json:"associated_data"`
}

// vaultResponse is the body of an encrypt or decrypt response.
type vaultResponse struct {
	Data struct {
		Plaintext  string `json:"plaintext"`
		Ciphertext string `json:"ciphertext"`
	} `json:"data"`
}

// Wrap implements Wrapper. The wrapped key is Vault's own ciphertext string (vault:v<n>:…), which
// names the version of the transit key that made it, so a rotated transit key still unwraps it.
func (w *vaultWrapper) Wrap(ctx context.Context, dataKey []byte, aad []byte) ([]byte, error) {
	var out vaultResponse

	in := vaultRequest{
		Plaintext:      base64.StdEncoding.EncodeToString(dataKey),
		AssociatedData: base64.StdEncoding.EncodeToString(aad),
	}

	if err := w.call(ctx, w.key, "encrypt", in, &out); err != nil {
		return nil, err
	}

	if !strings.HasPrefix(out.Data.Ciphertext, "vault:") {
		return nil, fmt.Errorf("vault transit encrypt under %s returned no ciphertext", w.key)
	}

	return []byte(out.Data.Ciphertext), nil
}

// Unwrap implements Wrapper.
func (w *vaultWrapper) Unwrap(ctx context.Context, wrapped []byte, key string, aad []byte) ([]byte, error) {
	var out vaultResponse

	in := vaultRequest{
		Ciphertext:     string(wrapped),
		AssociatedData: base64.StdEncoding.EncodeToString(aad),
	}

	if err := w.call(ctx, key, "decrypt", in, &out); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrUnwrap, err)
	}

	dataKey, err := base64.StdEncoding.DecodeString(out.Data.Plaintext)
	if err != nil || len(dataKey) == 0 {
		return nil, fmt.Errorf("vault transit decrypt under %s: %w: the reply holds no base64 plaintext", key, ErrUnwrap)
	}

	return dataKey, nil
}

// call makes one encrypt or decrypt call against a transit key.
func (w *vaultWrapper) call(ctx context.Context, key string, method string, in vaultRequest, out *vaultResponse) error {
	mount, name, err := vaultKeyPath(key)
	if err != nil {
		return err
	}

	url := w.endpoint + "/v1/" + mount + "/" + method + "/" + name

	if err := postJSON(ctx, w.client, url, map[string]string{"X-Vault-Token": w.token}, in, out); err != nil {
		return fmt.Errorf("vault transit %s under %s: %w", method, key, err)
	}

	return nil
}

// postJSON posts in as JSON and decodes a 2xx response into out. A failure reports the status and
// the start of the body - a KMS error names what is wrong (a denied permission, a missing key),
// and none of them echo key material.
func postJSON(ctx context.Context, client *http.Client, url string, headers map[string]string, in any, out any) error {
	body, err := json.Marshal(in)
	if err != nil {
		return fmt.Errorf("encoding the request: %w", err)
	}

	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("building the request: %w", err)
	}

	req.Header.Set("Content-Type", "application/json")

	for k, v := range headers {
		req.Header.Set(k, v)
	}

	resp, err := client.Do(req)
	if err != nil {
		return err
	}

	defer func() { _ = resp.Body.Close() }()

	payload, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return fmt.Errorf("reading the response: %w", err)
	}

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		const excerpt = 512

		text := strings.TrimSpace(string(payload))
		if len(text) > excerpt {
			text = text[:excerpt]
		}

		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, text)
	}

	if err := json.Unmarshal(payload, out); err != nil {
		return errors.Join(errors.New("decoding the response"), err)
	}

	return nil
}
