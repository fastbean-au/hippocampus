package contract

import (
	"context"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
)

// TestUnaryClientVersionInterceptor covers both halves: a version is attached, and an empty one
// attaches nothing rather than an empty header the service would have to tell apart from absence.
func TestUnaryClientVersionInterceptor(t *testing.T) {
	for _, version := range []string{"hippo/0.52.0", ""} {
		var sent []string

		invoker := func(ctx context.Context, _ string, _ any, _ any, _ *grpc.ClientConn, _ ...grpc.CallOption) error {
			md, _ := metadata.FromOutgoingContext(ctx)
			sent = md.Get(ClientVersionHeader)

			return nil
		}

		interceptor := UnaryClientVersionInterceptor(version)

		if err := interceptor(context.Background(), "/x/Y", nil, nil, nil, invoker); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		switch {

		case version == "" && len(sent) != 0:
			t.Errorf("expected no header for an empty version, got %v", sent)

		case version != "" && (len(sent) != 1 || sent[0] != version):
			t.Errorf("expected header %q, got %v", version, sent)

		}
	}
}
