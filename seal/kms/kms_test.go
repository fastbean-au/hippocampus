package kms

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// fakeSeal stands in for a KMS's encryption: the "ciphertext" is the additional data, a separator
// and the plaintext reversed, so a test can see that the wrapped form is not the key and that the
// additional data has to match for it to come back.
func fakeSeal(plaintext []byte, aad []byte) []byte {
	reversed := bytes.Clone(plaintext)

	for i, j := 0, len(reversed)-1; i < j; i, j = i+1, j-1 {
		reversed[i], reversed[j] = reversed[j], reversed[i]
	}

	return append(append(bytes.Clone(aad), 0), reversed...)
}

// fakeOpen reverses fakeSeal, failing on additional data that does not match.
func fakeOpen(ciphertext []byte, aad []byte) ([]byte, bool) {
	prefix := append(bytes.Clone(aad), 0)

	if !bytes.HasPrefix(ciphertext, prefix) {
		return nil, false
	}

	return fakeSeal(ciphertext[len(prefix):], nil)[1:], true
}

// dataKey draws a 32-byte key.
func dataKey(t *testing.T) []byte {
	t.Helper()

	key := make([]byte, 32)

	if _, err := rand.Read(key); err != nil {
		t.Fatalf("drawing a key: %s", err)
	}

	return key
}

// roundTrip wraps a key, unwraps it, and checks that other additional data does not unwrap it.
func roundTrip(t *testing.T, w Wrapper) {
	t.Helper()

	ctx := context.Background()
	key := dataKey(t)
	aad := []byte("hippocampus data key\x000a1b2c3d")

	wrapped, err := w.Wrap(ctx, key, aad)
	if err != nil {
		t.Fatalf("Wrap: %s", err)
	}

	if bytes.Contains(wrapped, key) {
		t.Fatal("the wrapped key contains the key")
	}

	got, err := w.Unwrap(ctx, wrapped, w.Key(), aad)
	if err != nil || !bytes.Equal(got, key) {
		t.Fatalf("Unwrap: %x, %v; want %x", got, err, key)
	}

	if _, err := w.Unwrap(ctx, wrapped, w.Key(), []byte("another row")); !errors.Is(err, ErrUnwrap) {
		t.Errorf("unwrapping under other additional data: %v, want ErrUnwrap", err)
	}
}

func TestVaultWrapsAndUnwraps(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Vault-Token") != "s.token" {
			http.Error(w, `{"errors":["permission denied"]}`, http.StatusForbidden)

			return
		}

		var in vaultRequest

		_ = json.NewDecoder(r.Body).Decode(&in)
		aad, _ := base64.StdEncoding.DecodeString(in.AssociatedData)

		var out vaultResponse

		switch r.URL.Path {

		case "/v1/ns1/transit/encrypt/hippocampus":
			plaintext, _ := base64.StdEncoding.DecodeString(in.Plaintext)
			out.Data.Ciphertext = "vault:v1:" + base64.StdEncoding.EncodeToString(fakeSeal(plaintext, aad))

		case "/v1/ns1/transit/decrypt/hippocampus":
			ciphertext, _ := base64.StdEncoding.DecodeString(strings.TrimPrefix(in.Ciphertext, "vault:v1:"))

			plaintext, ok := fakeOpen(ciphertext, aad)
			if !ok {
				http.Error(w, `{"errors":["cipher: message authentication failed"]}`, http.StatusBadRequest)

				return
			}

			out.Data.Plaintext = base64.StdEncoding.EncodeToString(plaintext)

		default:
			http.NotFound(w, r)

			return

		}

		_ = json.NewEncoder(w).Encode(out)
	}))

	defer server.Close()

	w, err := New(context.Background(), Config{Provider: "vault", Key: "ns1/transit/hippocampus", Endpoint: server.URL + "/", Token: "s.token"})
	if err != nil {
		t.Fatalf("New: %s", err)
	}

	roundTrip(t, w)

	denied, err := New(context.Background(), Config{Provider: "vault", Key: "ns1/transit/hippocampus", Endpoint: server.URL, Token: "wrong"})
	if err != nil {
		t.Fatalf("New: %s", err)
	}

	if _, err := denied.Wrap(context.Background(), dataKey(t), nil); err == nil || !strings.Contains(err.Error(), "permission denied") {
		t.Errorf("wrapping with a bad token: %v, want Vault's refusal", err)
	}
}

func TestGCPWrapsAndUnwraps(t *testing.T) {
	t.Parallel()

	const key = "projects/p/locations/global/keyRings/r/cryptoKeys/hippocampus"

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer ya29.token" {
			http.Error(w, `{"error":{"code":401}}`, http.StatusUnauthorized)

			return
		}

		var in gcpRequest

		_ = json.NewDecoder(r.Body).Decode(&in)
		aad, _ := base64.StdEncoding.DecodeString(in.AdditionalAuthenticatedData)

		var out gcpResponse

		switch r.URL.Path {

		case "/v1/" + key + ":encrypt":
			plaintext, _ := base64.StdEncoding.DecodeString(in.Plaintext)
			out.Ciphertext = base64.StdEncoding.EncodeToString(fakeSeal(plaintext, aad))

		case "/v1/" + key + ":decrypt":
			ciphertext, _ := base64.StdEncoding.DecodeString(in.Ciphertext)

			plaintext, ok := fakeOpen(ciphertext, aad)
			if !ok {
				http.Error(w, `{"error":{"code":400,"message":"Decryption failed"}}`, http.StatusBadRequest)

				return
			}

			out.Plaintext = base64.StdEncoding.EncodeToString(plaintext)

		default:
			http.NotFound(w, r)

			return

		}

		_ = json.NewEncoder(w).Encode(out)
	}))

	defer server.Close()

	w, err := New(context.Background(), Config{Provider: "gcp", Key: key, Endpoint: server.URL, Token: "ya29.token"})
	if err != nil {
		t.Fatalf("New: %s", err)
	}

	roundTrip(t, w)
}

// TestAWSWrapsAndUnwraps speaks the AWS JSON protocol KMS uses, so it runs the real SDK client
// against a fake endpoint. Not parallel: the SDK's credentials and region come from the
// environment, which t.Setenv sets for the whole process.
func TestAWSWrapsAndUnwraps(t *testing.T) {
	const key = "arn:aws:kms:ap-southeast-2:111122223333:key/1234abcd-12ab-34cd-56ef-1234567890ab"

	t.Setenv("AWS_ACCESS_KEY_ID", "AKIDEXAMPLE")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "secret")
	t.Setenv("AWS_CONFIG_FILE", t.TempDir()+"/none")
	t.Setenv("AWS_SHARED_CREDENTIALS_FILE", t.TempDir()+"/none")

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.Header.Get("Authorization"), "/ap-southeast-2/kms/") {
			http.Error(w, `{"__type":"UnrecognizedClientException"}`, http.StatusBadRequest)

			return
		}

		var in struct {
			KeyId             string
			Plaintext         []byte
			CiphertextBlob    []byte
			EncryptionContext map[string]string
		}

		_ = json.NewDecoder(r.Body).Decode(&in)
		aad := []byte(in.EncryptionContext[awsContextKey])

		w.Header().Set("Content-Type", "application/x-amz-json-1.1")

		switch r.Header.Get("X-Amz-Target") {

		case "TrentService.Encrypt":
			_ = json.NewEncoder(w).Encode(map[string]any{"KeyId": in.KeyId, "CiphertextBlob": fakeSeal(in.Plaintext, aad)})

		case "TrentService.Decrypt":
			plaintext, ok := fakeOpen(in.CiphertextBlob, aad)
			if !ok || in.KeyId != key {
				w.WriteHeader(http.StatusBadRequest)
				_, _ = w.Write([]byte(`{"__type":"InvalidCiphertextException","message":"no"}`))

				return
			}

			_ = json.NewEncoder(w).Encode(map[string]any{"KeyId": in.KeyId, "Plaintext": plaintext})

		default:
			http.NotFound(w, r)

		}
	}))

	defer server.Close()

	w, err := New(context.Background(), Config{Provider: "aws", Key: key, Endpoint: server.URL})
	if err != nil {
		t.Fatalf("New: %s", err)
	}

	roundTrip(t, w)
}

func TestConfigProblems(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		cfg  Config
		want string
	}{
		{name: "unknown provider", cfg: Config{Provider: "azure", Key: "k"}, want: "not one of"},
		{name: "no key", cfg: Config{Provider: "aws"}, want: "needs storage.encryption.kms.keyId"},
		{name: "vault without an address", cfg: Config{Provider: "vault", Key: "transit/k", Token: "t"}, want: "the Vault address"},
		{name: "vault without a token", cfg: Config{Provider: "vault", Key: "transit/k", Endpoint: "http://vault"}, want: "kms.token"},
		{name: "vault key without a mount", cfg: Config{Provider: "vault", Key: "k", Endpoint: "http://vault", Token: "t"}, want: "<mount>/<key>"},
		{name: "gcp key that is not a resource name", cfg: Config{Provider: "gcp", Key: "hippocampus"}, want: "not a Google Cloud KMS key name"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			problems := test.cfg.Problems()

			if len(problems) == 0 || !strings.Contains(errors.Join(problems...).Error(), test.want) {
				t.Errorf("Problems() = %v, want one containing %q", problems, test.want)
			}

			if _, err := New(context.Background(), test.cfg); err == nil {
				t.Error("New accepted the configuration")
			}
		})
	}

	valid := []Config{
		{Provider: "aws", Key: "alias/hippocampus"},
		{Provider: "gcp", Key: "projects/p/locations/l/keyRings/r/cryptoKeys/k"},
		{Provider: "vault", Key: "transit/hippocampus", Endpoint: "https://vault:8200", Token: "t"},
	}

	for _, cfg := range valid {
		if problems := cfg.Problems(); len(problems) != 0 {
			t.Errorf("%+v: %v", cfg, problems)
		}
	}
}

func TestAWSRegionFromARN(t *testing.T) {
	t.Parallel()

	cases := map[string]string{
		"arn:aws:kms:us-east-1:111122223333:key/abc":       "us-east-1",
		"arn:aws:kms:eu-west-2:111122223333:alias/example": "eu-west-2",
		"alias/example":       "",
		"1234abcd-12ab":       "",
		"arn:aws:s3:::bucket": "",
	}

	for k, v := range cases {
		if got := awsRegionFromARN(k); got != v {
			t.Errorf("awsRegionFromARN(%q) = %q, want %q", k, got, v)
		}
	}
}

// TestProvidersNameThemselves pins each wrapper's name, which is what the ledger records beside the
// data keys it wrapped.
func TestProvidersNameThemselves(t *testing.T) {
	t.Parallel()

	wrappers := map[string]Wrapper{
		"aws":   &awsWrapper{},
		"gcp":   &gcpWrapper{},
		"vault": &vaultWrapper{},
	}

	for k, v := range wrappers {
		if v.Provider() != k {
			t.Errorf("the %s wrapper names itself %q", k, v.Provider())
		}
	}
}

// TestRESTProvidersReportAKMSThatMisbehaves covers what a KMS can send back short of an answer: an
// error status, a body that is not JSON, a reply with nothing in it, and a plaintext that is not
// base64. Each is an error, and none of them a key.
func TestRESTProvidersReportAKMSThatMisbehaves(t *testing.T) {
	t.Parallel()

	replies := map[string]string{
		"an error status": "",
		"not JSON":        "{",
		"an empty reply":  "{}",
		"a bad plaintext": `{"plaintext":"!!","ciphertext":"!!","data":{"plaintext":"!!","ciphertext":"vault:v1:x"}}`,
	}

	for name, reply := range replies {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				if reply == "" {
					http.Error(w, strings.Repeat("denied ", 200), http.StatusForbidden)

					return
				}

				_, _ = w.Write([]byte(reply))
			}))

			t.Cleanup(server.Close)

			configs := []Config{
				{Provider: "gcp", Key: "projects/p/locations/l/keyRings/r/cryptoKeys/k", Endpoint: server.URL, Token: "t"},
				{Provider: "vault", Key: "transit/k", Endpoint: server.URL, Token: "t"},
			}

			for _, cfg := range configs {
				w, err := New(context.Background(), cfg)
				if err != nil {
					t.Fatalf("New(%s): %s", cfg.Provider, err)
				}

				_, wrapErr := w.Wrap(context.Background(), dataKey(t), nil)

				_, unwrapErr := w.Unwrap(context.Background(), []byte("vault:v1:x"), cfg.Key, nil)
				if !errors.Is(unwrapErr, ErrUnwrap) {
					t.Errorf("%s Unwrap: %v, want ErrUnwrap", cfg.Provider, unwrapErr)
				}

				// A vault reply carrying a ciphertext is a successful wrap; every other reply here
				// is a failed one.
				wrapped := cfg.Provider == "vault" && name == "a bad plaintext"

				if wrapErr == nil && !wrapped {
					t.Errorf("%s Wrap succeeded on %s", cfg.Provider, name)
				}
			}
		})
	}
}

// TestRESTProvidersReportAnUnreachableKMS covers a KMS nothing answers on, and an endpoint that is
// not a URL at all.
func TestRESTProvidersReportAnUnreachableKMS(t *testing.T) {
	t.Parallel()

	for _, endpoint := range []string{"http://127.0.0.1:1", "http://bad\x7fhost"} {
		w, err := New(context.Background(), Config{Provider: "vault", Key: "transit/k", Endpoint: endpoint, Token: "t"})
		if err != nil {
			t.Fatalf("New: %s", err)
		}

		if _, err := w.Wrap(context.Background(), dataKey(t), nil); err == nil {
			t.Errorf("wrapping against %q succeeded", endpoint)
		}
	}

	w := &vaultWrapper{endpoint: "http://127.0.0.1:1", client: http.DefaultClient}

	if _, err := w.Unwrap(context.Background(), nil, "no-mount", nil); err == nil || !strings.Contains(err.Error(), "<mount>/<key>") {
		t.Errorf("unwrapping under a key with no mount: %v", err)
	}
}

// TestGCPWithoutCredentials is a start with no token and no application default credentials to
// fall back on: it fails when the wrapper is built, not at the first call. Not parallel: it points
// the credential search at a file that does not exist, through the environment.
func TestGCPWithoutCredentials(t *testing.T) {
	t.Setenv("GOOGLE_APPLICATION_CREDENTIALS", t.TempDir()+"/missing.json")

	_, err := New(context.Background(), Config{Provider: "gcp", Key: "projects/p/locations/l/keyRings/r/cryptoKeys/k"})
	if err == nil || !strings.Contains(err.Error(), "application default credentials") {
		t.Errorf("New without credentials: %v", err)
	}
}

// TestAWSReportsARefusedEncrypt covers an AWS KMS that refuses to wrap. Not parallel, for the reason
// TestAWSWrapsAndUnwraps gives.
func TestAWSReportsARefusedEncrypt(t *testing.T) {
	t.Setenv("AWS_ACCESS_KEY_ID", "AKIDEXAMPLE")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "secret")
	t.Setenv("AWS_REGION", "ap-southeast-2")
	t.Setenv("AWS_CONFIG_FILE", t.TempDir()+"/none")
	t.Setenv("AWS_SHARED_CREDENTIALS_FILE", t.TempDir()+"/none")

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/x-amz-json-1.1")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"__type":"AccessDeniedException","message":"no"}`))
	}))

	defer server.Close()

	w, err := New(context.Background(), Config{Provider: "aws", Key: "alias/hippocampus", Endpoint: server.URL})
	if err != nil {
		t.Fatalf("New: %s", err)
	}

	if _, err := w.Wrap(context.Background(), dataKey(t), nil); err == nil || !strings.Contains(err.Error(), "AccessDenied") {
		t.Errorf("Wrap against a refusing KMS: %v", err)
	}
}
