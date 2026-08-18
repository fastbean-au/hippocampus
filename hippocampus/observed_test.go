package hippocampus

import (
	"context"
	"maps"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/spf13/viper"
	"google.golang.org/grpc"

	"github.com/fastbean-au/hippocampus/auth"
	"github.com/fastbean-au/hippocampus/contract"
)

// observedContext is a request context carrying verified claims, as both enforcement adapters leave
// it for anything downstream of them.
func observedContext(clientId string, roles []string, groups []string) context.Context {
	return auth.ContextWithClaims(context.Background(), &auth.Claims{
		ClientID: clientId,
		Roles:    roles,
		Groups:   groups,
	})
}

func observedRPCInfo() *grpc.UnaryServerInfo {
	return &grpc.UnaryServerInfo{FullMethod: hippocampusServicePrefix + "StoreMemory"}
}

func passthroughHandler(_ context.Context, _ interface{}) (interface{}, error) {
	return nil, nil
}

// TestObservedCallerRecordedFromVerifiedClaims is the base case: a client calling with a token is
// drawn, sourced as OBSERVED, with an edge pointing INWARD. The direction is the assertion that
// matters - every other edge in the graph runs outward from this instance, and a caller reversed
// would turn the one thing this half of the view adds into a claim that the service dials its own
// clients.
func TestObservedCallerRecordedFromVerifiedClaims(t *testing.T) {
	s := newTopologyServer(t)

	viper.Set("auth.method", "hmac")

	if _, err := s.InterceptorObserveCaller(
		observedContext("nats-bridge", []string{"writer"}, nil),
		nil, observedRPCInfo(), passthroughHandler,
	); err != nil {
		t.Fatalf("InterceptorObserveCaller: %s", err)
	}

	res, err := s.GetTopology(context.Background(), &contract.EmptyRequest{})
	if err != nil {
		t.Fatalf("GetTopology: %s", err)
	}

	nodes := nodesById(res)

	node, ok := nodes["observed:nats-bridge"]
	if !ok {
		t.Fatalf("the observed caller has no node; got %v", slices.Sorted(maps.Keys(nodes)))
	}

	if node.GetSource() != contract.TopologyNodeSource_TOPOLOGY_NODE_SOURCE_OBSERVED {
		t.Errorf("source = %s, want OBSERVED", node.GetSource())
	}

	if node.GetKind() != contract.TopologyNodeKind_TOPOLOGY_NODE_KIND_CLIENT {
		t.Errorf("kind = %s, want CLIENT", node.GetKind())
	}

	// No health, permanently: a call proves the client was alive at that instant, which is not the
	// same claim as a probe's, and the two must not render alike.
	if node.GetStatus() != contract.TopologyStatus_TOPOLOGY_STATUS_UNSPECIFIED {
		t.Errorf("status = %s, want UNSPECIFIED - an observed caller carries no health", node.GetStatus())
	}

	if node.GetCheckedAt() != 0 {
		t.Errorf("checked_at = %d, want 0 - nothing probed this node", node.GetCheckedAt())
	}

	// It holds no address for a caller, and inventing one would be the first step towards the
	// control plane this view is careful not to be.
	if node.GetDetail() != "" {
		t.Errorf("detail = %q, want empty", node.GetDetail())
	}

	if got := attributeValue(node, "roles"); got != "writer" {
		t.Errorf("roles = %q, want %q", got, "writer")
	}

	if got := attributeValue(node, "transport"); got != "grpc" {
		t.Errorf("transport = %q, want %q", got, "grpc")
	}

	if got := attributeValue(node, "calls"); got != "1" {
		t.Errorf("calls = %q, want %q", got, "1")
	}

	var inbound bool

	for _, edge := range res.GetEdges() {
		if edge.GetFromId() == "observed:nats-bridge" && edge.GetToId() == topologyNodeSelf {
			inbound = true
		}

		if edge.GetFromId() == topologyNodeSelf && edge.GetToId() == "observed:nats-bridge" {
			t.Error("the caller's edge points outward: this instance does not dial its callers")
		}
	}

	if !inbound {
		t.Error("no edge from the observed caller to this instance")
	}
}

// TestObservedCallerRecordsNothingWithoutAClientId covers the two ways there is nothing honest to
// draw: authentication is off, so no request carries claims at all, and a verified token that names
// no client. Neither is inferred from an address or a user agent - a source address names a proxy
// and a user agent names whatever the caller typed.
func TestObservedCallerRecordsNothingWithoutAClientId(t *testing.T) {
	s := newTopologyServer(t)

	for name, ctx := range map[string]context.Context{
		"no claims at all":      context.Background(),
		"claims with no client": auth.ContextWithClaims(context.Background(), &auth.Claims{Roles: []string{"reader"}}),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := s.InterceptorObserveCaller(ctx, nil, observedRPCInfo(), passthroughHandler); err != nil {
				t.Fatalf("InterceptorObserveCaller: %s", err)
			}

			records, _ := s.observed.snapshot()

			if len(records) != 0 {
				t.Errorf("recorded %d caller(s), want none", len(records))
			}
		})
	}
}

// TestObservedCallerSelfAttributeExplainsAnEmptyColumn is the reason the count is reported at all.
// An operator with six bridges writing to an unauthenticated instance sees no caller boxes, and
// must be told that callers are not identified rather than left to conclude the view is broken.
func TestObservedCallerSelfAttributeExplainsAnEmptyColumn(t *testing.T) {
	s := newTopologyServer(t)

	for name, tc := range map[string]struct {
		method string
		want   string
	}{
		"authentication off": {method: "none", want: "not identified (authentication is disabled: auth.method)"},
		"authentication on":  {method: "hmac", want: "0 (none has called since this instance started)"},
	} {
		t.Run(name, func(t *testing.T) {
			viper.Set("auth.method", tc.method)

			res, err := s.GetTopology(context.Background(), &contract.EmptyRequest{})
			if err != nil {
				t.Fatalf("GetTopology: %s", err)
			}

			if got := attributeValue(nodesById(res)[topologyNodeSelf], "observed_callers"); got != tc.want {
				t.Errorf("observed_callers = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestObservedCallerMergesIntoADeclaredComponent covers the correlation that makes the two inbound
// halves worth having together. An operator who declares "nats-bridge" and mints it a token of the
// same name should see ONE box - and the merged box separates the two cases nothing else in the
// view can: a bridge that is up and writing, and a bridge that is up and has never written.
func TestObservedCallerMergesIntoADeclaredComponent(t *testing.T) {
	s := declaredServer(t, TopologyComponent{
		Name:      "nats-bridge",
		Kind:      "bridge",
		HealthURL: "http://nats-bridge:8090",
	})

	viper.Set("auth.method", "hmac")

	ctx := observedContext("nats-bridge", []string{"writer"}, nil)

	for range 3 {
		if _, err := s.InterceptorObserveCaller(ctx, nil, observedRPCInfo(), passthroughHandler); err != nil {
			t.Fatalf("InterceptorObserveCaller: %s", err)
		}
	}

	res, err := s.GetTopology(context.Background(), &contract.EmptyRequest{})
	if err != nil {
		t.Fatalf("GetTopology: %s", err)
	}

	nodes := nodesById(res)

	if _, ok := nodes["observed:nats-bridge"]; ok {
		t.Error("the declared component was drawn twice: once declared, once observed")
	}

	declared, ok := nodes["declared:nats-bridge"]
	if !ok {
		t.Fatalf("the declared bridge has no node; got %v", slices.Sorted(maps.Keys(nodes)))
	}

	if got := attributeValue(declared, "calls"); got != "3" {
		t.Errorf("calls = %q, want %q", got, "3")
	}

	if attributeValue(declared, "last_call") == "" {
		t.Error("the declared component carries no last_call, so a bridge that is up but silent reads as a healthy one")
	}
}

// TestObservedCallersAreBounded is the security property: the map is keyed on a value that arrives
// in a token, so without a cap it is memory a caller controls. The oldest entry goes, and the view
// says it is showing a subset rather than presenting a truncated list as complete.
func TestObservedCallersAreBounded(t *testing.T) {
	s := newTopologyServer(t)

	viper.Set("auth.method", "hmac")

	now := time.Now()

	for i := range maxObservedCallers + 10 {
		// Ascending last-seen, so the first client inserted is the one eviction should choose.
		s.observed.record("client-"+strconv.Itoa(i), []string{"reader"}, false, observedTransportGRPC,
			now.Add(time.Duration(i)*time.Second))
	}

	records, evicted := s.observed.snapshot()

	if len(records) != maxObservedCallers {
		t.Errorf("registry holds %d entries, want the cap of %d", len(records), maxObservedCallers)
	}

	if !evicted {
		t.Error("the registry evicted entries but does not report having done so")
	}

	for _, record := range records {
		if record.id == "client-0" {
			t.Error("the least recently seen entry survived eviction")
		}
	}

	res, err := s.GetTopology(context.Background(), &contract.EmptyRequest{})
	if err != nil {
		t.Fatalf("GetTopology: %s", err)
	}

	want := strconv.Itoa(maxObservedCallers) + " (capped; showing the most recently seen)"

	if got := attributeValue(nodesById(res)[topologyNodeSelf], "observed_callers"); got != want {
		t.Errorf("observed_callers = %q, want %q", got, want)
	}
}

// TestObservedCallerAccumulatesBothTransports covers one client using the CLI over gRPC and a
// browser over the gateway. That is one client, and reporting only the most recent surface would
// make the row flap between two values that are both true.
func TestObservedCallerAccumulatesBothTransports(t *testing.T) {
	s := newTopologyServer(t)

	ctx := observedContext("hippo-cli", []string{"admin"}, nil)

	if _, err := s.InterceptorObserveCaller(ctx, nil, observedRPCInfo(), passthroughHandler); err != nil {
		t.Fatalf("InterceptorObserveCaller: %s", err)
	}

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/v1/memories", nil).WithContext(ctx)

	s.HTTPMiddlewareObserveCaller(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})).ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("the middleware did not pass the request through: %d", recorder.Code)
	}

	records, _ := s.observed.snapshot()

	if len(records) != 1 {
		t.Fatalf("recorded %d callers, want 1 - the two transports are one client", len(records))
	}

	if got := transportDescription(records[0].transports); got != "grpc, http" {
		t.Errorf("transport = %q, want %q", got, "grpc, http")
	}

	if records[0].calls != 2 {
		t.Errorf("calls = %d, want 2", records[0].calls)
	}
}

// TestObservedCallerIgnoresNonServiceRPCs keeps the health surface out. An orchestrator probing
// grpc.health.v1.Health is not a client of the store, and counting it would put a box on the
// diagram for every deployment that has a liveness probe - which is all of them.
func TestObservedCallerIgnoresNonServiceRPCs(t *testing.T) {
	s := newTopologyServer(t)

	info := &grpc.UnaryServerInfo{FullMethod: "/grpc.health.v1.Health/Check"}

	if _, err := s.InterceptorObserveCaller(
		observedContext("kubelet", []string{"reader"}, nil), nil, info, passthroughHandler,
	); err != nil {
		t.Fatalf("InterceptorObserveCaller: %s", err)
	}

	if records, _ := s.observed.snapshot(); len(records) != 0 {
		t.Errorf("recorded %d caller(s) from a health check, want none", len(records))
	}
}

// TestObservedCallerRecordsNothingWhenTheViewIsOff pairs the registry's lifetime with the feature
// it feeds: with topology.enabled false there is nothing to render, so nothing should be retained
// about who called either.
func TestObservedCallerRecordsNothingWhenTheViewIsOff(t *testing.T) {
	s := newTopologyServer(t)
	s.topology.enabled = false

	if _, err := s.InterceptorObserveCaller(
		observedContext("nats-bridge", []string{"writer"}, nil), nil, observedRPCInfo(), passthroughHandler,
	); err != nil {
		t.Fatalf("InterceptorObserveCaller: %s", err)
	}

	if records, _ := s.observed.snapshot(); len(records) != 0 {
		t.Errorf("recorded %d caller(s) with the view disabled, want none", len(records))
	}
}

// TestObservedCallerReportsScopeWithoutNamingGroups is a disclosure test. The topology is visible at
// reader tier by default and a group name is frequently a customer's, so whether a caller is bound
// may be reported and what it is bound TO may not.
func TestObservedCallerReportsScopeWithoutNamingGroups(t *testing.T) {
	s := newTopologyServer(t)

	if _, err := s.InterceptorObserveCaller(
		observedContext("tenant-writer", []string{"writer"}, []string{"acme-corp"}),
		nil, observedRPCInfo(), passthroughHandler,
	); err != nil {
		t.Fatalf("InterceptorObserveCaller: %s", err)
	}

	res, err := s.GetTopology(context.Background(), &contract.EmptyRequest{})
	if err != nil {
		t.Fatalf("GetTopology: %s", err)
	}

	node := nodesById(res)["observed:tenant-writer"]

	if node == nil {
		t.Fatal("the observed caller has no node")
	}

	if got := attributeValue(node, "group_scoped"); got != "yes" {
		t.Errorf("group_scoped = %q, want %q", got, "yes")
	}

	for _, attribute := range node.GetAttributes() {
		if attribute.GetValue() == "acme-corp" {
			t.Errorf("attribute %q names the caller's group, disclosing another partition", attribute.GetKey())
		}
	}
}

// TestObservedCallerIdentityFollowsTheToken covers a client re-minted with different roles. Pinning
// the first identity seen would leave the view describing a token that is no longer in use, which
// is worse than not reporting roles at all.
func TestObservedCallerIdentityFollowsTheToken(t *testing.T) {
	s := newTopologyServer(t)

	for _, roles := range [][]string{{"reader"}, {"reader", "writer"}} {
		if _, err := s.InterceptorObserveCaller(
			observedContext("promoted", roles, nil), nil, observedRPCInfo(), passthroughHandler,
		); err != nil {
			t.Fatalf("InterceptorObserveCaller: %s", err)
		}
	}

	records, _ := s.observed.snapshot()

	if len(records) != 1 {
		t.Fatalf("recorded %d callers, want 1", len(records))
	}

	if got := rolesDescription(records[0].roles); got != "reader, writer" {
		t.Errorf("roles = %q, want %q", got, "reader, writer")
	}
}

// TestObservedCallersConcurrent is the one that has to hold: this registry is written from every
// authenticated request's own goroutine, which is why the entry's moving fields are atomics and the
// lock is taken only to insert. Run under -race.
func TestObservedCallersConcurrent(t *testing.T) {
	s := newTopologyServer(t)

	var wg sync.WaitGroup

	for range 8 {
		wg.Add(1)

		go func() {
			defer wg.Done()

			for j := range 50 {
				s.observed.record("client-"+strconv.Itoa(j%4), []string{"writer"}, false,
					observedTransportGRPC, time.Now())
			}

			_, _ = s.observed.snapshot()
		}()
	}

	wg.Wait()

	records, _ := s.observed.snapshot()

	if len(records) != 4 {
		t.Fatalf("recorded %d callers, want 4 - concurrent first calls split one client across entries", len(records))
	}

	var total int64

	for _, record := range records {
		total += record.calls
	}

	if total != 8*50 {
		t.Errorf("counted %d calls, want %d", total, 8*50)
	}
}

// TestObservedValueRenderers pins the small strings an operator actually reads, including the two
// that are only correct because they spell out a zero: a caller with no roles is refused every RPC,
// and a moment that never happened is not "0s ago".
func TestObservedValueRenderers(t *testing.T) {
	now := time.Now()

	if got := transportDescription(0); got != "unknown" {
		t.Errorf("transportDescription(0) = %q, want %q", got, "unknown")
	}

	if got := rolesDescription(nil); got != "none" {
		t.Errorf("rolesDescription(nil) = %q, want %q", got, "none")
	}

	if got := scopedDescription(false); got != "no" {
		t.Errorf("scopedDescription(false) = %q, want %q", got, "no")
	}

	if got := agoDescription(now, time.Time{}); got != "never" {
		t.Errorf("agoDescription(zero) = %q, want %q", got, "never")
	}

	if got := agoDescription(now, now.Add(-90*time.Second)); got != "1m30s ago" {
		t.Errorf("agoDescription(90s) = %q, want %q", got, "1m30s ago")
	}

	// A clock that stepped backwards must not produce a negative age, which reads as a caller that
	// has not called yet.
	if got := agoDescription(now, now.Add(time.Minute)); got != "0s ago" {
		t.Errorf("agoDescription(future) = %q, want %q", got, "0s ago")
	}
}

// TestObservedEdgesTerminateOnNodes extends the graph's own invariant to the nodes this file adds:
// an edge to nothing draws a line into empty space.
func TestObservedEdgesTerminateOnNodes(t *testing.T) {
	s := newTopologyServer(t)

	if _, err := s.InterceptorObserveCaller(
		observedContext("hippo-cli", []string{"admin"}, nil), nil, observedRPCInfo(), passthroughHandler,
	); err != nil {
		t.Fatalf("InterceptorObserveCaller: %s", err)
	}

	res, err := s.GetTopology(context.Background(), &contract.EmptyRequest{})
	if err != nil {
		t.Fatalf("GetTopology: %s", err)
	}

	nodes := nodesById(res)

	for _, edge := range res.GetEdges() {
		if _, ok := nodes[edge.GetFromId()]; !ok {
			t.Errorf("edge %s -> %s starts on a node that is not in the response", edge.GetFromId(), edge.GetToId())
		}

		if _, ok := nodes[edge.GetToId()]; !ok {
			t.Errorf("edge %s -> %s ends on a node that is not in the response", edge.GetFromId(), edge.GetToId())
		}
	}
}
