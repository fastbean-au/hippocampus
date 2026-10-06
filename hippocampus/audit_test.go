package hippocampus

import (
	"bytes"
	"context"
	"reflect"
	"strings"
	"testing"

	log "github.com/sirupsen/logrus"

	"github.com/fastbean-au/hippocampus/auth"
	"github.com/fastbean-au/hippocampus/contract"
	"github.com/fastbean-au/hippocampus/types"
)

// The audit trail for administrative mutations (TODO-3 item 169). A Purge, a predicate delete or a
// Clear was logged at Trace if at all, so the one record of who emptied a store was the absence of
// the store.

func captureLog(t *testing.T) *bytes.Buffer {
	t.Helper()

	var buf bytes.Buffer

	previousOut := log.StandardLogger().Out
	previousLevel := log.GetLevel()
	previousFormatter := log.StandardLogger().Formatter

	log.SetOutput(&buf)
	log.SetLevel(log.InfoLevel)
	log.SetFormatter(&log.TextFormatter{DisableColors: true, DisableTimestamp: true})

	t.Cleanup(func() {
		log.SetOutput(previousOut)
		log.SetLevel(previousLevel)
		log.SetFormatter(previousFormatter)
	})

	return &buf
}

// TestEveryAdminMutationIsAudited calls each RPC the policy table marks as an admin-tier mutation
// through the audited server, by name, and requires an audit line for it. Being driven from the
// policy table is what makes it a guard: a new administrative RPC fails here until it is audited.
func TestEveryAdminMutationIsAudited(t *testing.T) {
	audited := reflect.ValueOf(Audited(newTestServer(t)))

	for _, rpc := range auth.AdminMutations() {
		t.Run(rpc, func(t *testing.T) {
			method := audited.MethodByName(rpc)
			if !method.IsValid() {
				t.Fatalf("the audited server has no method %s", rpc)
			}

			buf := captureLog(t)

			request := reflect.New(method.Type().In(1).Elem())
			ctx := auth.ContextWithClaims(context.Background(), &auth.Claims{ClientID: "ops-console"})

			method.Call([]reflect.Value{reflect.ValueOf(ctx), request})

			line := buf.String()

			for _, want := range []string{"audit=true", "rpc=" + rpc, "client_id=ops-console", "outcome="} {
				if !strings.Contains(line, want) {
					t.Errorf("no %q in the audit output %q", want, line)
				}
			}

			if !strings.Contains(line, "level=info") {
				t.Errorf("the audit line is not at Info: %q", line)
			}
		})
	}
}

// TestTheAuditLineCarriesTheRequestAndTheResult: the filter a predicate delete ran with and what it
// removed are the two facts an audit trail exists for.
func TestTheAuditLineCarriesTheRequestAndTheResult(t *testing.T) {
	s := newTestServer(t)
	seedIdMemories(t, s, types.Memory{Id: "m1", Group: "acme"}, types.Memory{Id: "m2", Group: "other"})

	buf := captureLog(t)

	if _, err := Audited(s).DeleteMemoriesByFilter(context.Background(), &contract.DeleteMemoriesByFilterRequest{Group: "acme"}); err != nil {
		t.Fatalf("DeleteMemoriesByFilter: %s", err)
	}

	line := buf.String()

	for _, want := range []string{"outcome=OK", `acme`, "memoriesDeleted", "client_id=none"} {
		if !strings.Contains(line, want) {
			t.Errorf("no %q in %q", want, line)
		}
	}
}

// TestARefusedCallIsAuditedWithItsCode: an attempt is part of the trail, not only a success.
func TestARefusedCallIsAuditedWithItsCode(t *testing.T) {
	buf := captureLog(t)

	_, _ = Audited(newTestServer(t)).DeleteMemoriesByFilter(context.Background(), &contract.DeleteMemoriesByFilterRequest{})

	if line := buf.String(); !strings.Contains(line, "outcome=InvalidArgument") {
		t.Errorf("a refused call's audit line is %q, want outcome=InvalidArgument", line)
	}
}
