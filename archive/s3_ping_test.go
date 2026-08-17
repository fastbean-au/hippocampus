package archive

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestS3Store_Ping drives the deployment-topology probe against an in-memory endpoint. The two
// cases are the two mistakes that actually happen with an object store - a bucket that is not
// there, and credentials that cannot see it - and both surface today only when somebody runs an
// Export, which is the worst possible moment to find out.
func TestS3Store_Ping(t *testing.T) {
	t.Setenv("AWS_ACCESS_KEY_ID", "test")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "test")
	t.Setenv("AWS_REGION", "us-east-1")

	for name, tc := range map[string]struct {
		status  int
		wantErr bool
	}{
		"the bucket is there":       {status: http.StatusOK, wantErr: false},
		"the bucket does not exist": {status: http.StatusNotFound, wantErr: true},
		"the credentials cannot see it": {
			status:  http.StatusForbidden,
			wantErr: true,
		},
	} {
		t.Run(name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodHead {
					t.Errorf("Ping issued a %s; it must transfer no object", r.Method)
				}

				w.WriteHeader(tc.status)
			}))

			t.Cleanup(server.Close)

			store, err := NewS3Store(context.Background(), S3Config{
				Bucket:       "archive-bucket",
				Region:       "us-east-1",
				Endpoint:     server.URL,
				UsePathStyle: true,
			})
			if err != nil {
				t.Fatalf("NewS3Store: %s", err)
			}

			err = store.Ping(context.Background())

			if tc.wantErr && err == nil {
				t.Error("Ping reported success")
			}

			if !tc.wantErr && err != nil {
				t.Errorf("Ping: %s", err)
			}
		})
	}
}

// TestS3Store_PingUnreachable covers the endpoint that is not answering at all, which must be an
// error rather than a hang - the probe runs on a timer and shares its round with every other
// dependency.
func TestS3Store_PingUnreachable(t *testing.T) {
	t.Setenv("AWS_ACCESS_KEY_ID", "test")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "test")
	t.Setenv("AWS_REGION", "us-east-1")

	store, err := NewS3Store(context.Background(), S3Config{
		Bucket:       "archive-bucket",
		Region:       "us-east-1",
		Endpoint:     "http://127.0.0.1:1",
		UsePathStyle: true,
	})
	if err != nil {
		t.Fatalf("NewS3Store: %s", err)
	}

	if err := store.Ping(context.Background()); err == nil {
		t.Error("Ping reported success against an endpoint with nothing listening")
	}
}
