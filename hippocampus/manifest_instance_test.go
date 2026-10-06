package hippocampus

import (
	"context"
	"strings"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/fastbean-au/hippocampus/contract"
)

// Export manifests live in the memory of the instance that produced them (TODO-3 item 169), so
// behind a load balancer a deferred Clear reaching another replica found nothing - and said only
// "unknown manifest", which reads as an expired id rather than a misrouted call.

func TestExportNamesTheInstanceHoldingTheManifest(t *testing.T) {
	objects := newFakeObjectStore()
	s := newTransferTestServer(t, objects)
	s.topology.instanceId = "replica-a:50051"

	res, err := s.Export(context.Background(), &contract.ExportRequest{})
	if err != nil {
		t.Fatalf("Export: %s", err)
	}

	if res.GetInstanceId() != "replica-a:50051" {
		t.Errorf("instance_id = %q, want the instance holding the manifest", res.GetInstanceId())
	}
}

func TestClearOfAnUnknownManifestSaysWhereManifestsLive(t *testing.T) {
	s := newTransferTestServer(t, newFakeObjectStore())
	s.topology.instanceId = "replica-b:50051"

	_, err := s.Clear(context.Background(), &contract.ClearRequest{ManifestId: "from-another-replica"})
	if status.Code(err) != codes.NotFound {
		t.Fatalf("Clear = %v, want NotFound", err)
	}

	message := status.Convert(err).Message()

	for _, want := range []string{"replica-b:50051", "instance_id", "memory"} {
		if !strings.Contains(message, want) {
			t.Errorf("the refusal %q does not mention %q", message, want)
		}
	}
}
