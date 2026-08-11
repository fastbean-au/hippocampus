package observability

import (
	"sync/atomic"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

// group holds the configured tenancy label, set once by Init and read by every recording site. It
// is atomic because Init runs on the main goroutine while recordings happen on whichever goroutine
// is doing the work; in practice Init is long finished first, but a package-level var read from
// several goroutines is a data race whether or not it is ever observed.
var group atomic.Pointer[attribute.KeyValue]

// WithGroup builds the measurement option for a recording, adding the tenancy attribute when one is
// configured.
//
// The group is stamped on the metrics as an INSTRUMENT attribute as well as a resource attribute,
// and the reason is practical rather than aesthetic: the OTLP-to-Prometheus translation promotes
// only service.name/service.version/job/instance onto each series and puts every other resource
// attribute in `target_info`, so slicing by tenant would otherwise need
//
//	hippocampus_ingestor_events_total * on(job) group_left(hippocampus_group) target_info
//
// on every query and every alert rule. Costing that out: the value is fixed for the lifetime of the
// process, so it multiplies the series count by exactly one - it carries none of the cardinality
// risk that reading a group off each record would, which is why that is the shape this is allowed to
// take. See GroupAttribute.
func WithGroup(attrs ...attribute.KeyValue) metric.MeasurementOption {
	if kv := group.Load(); kv != nil {
		attrs = append(attrs, *kv)
	}

	return metric.WithAttributes(attrs...)
}

// setGroup records the tenancy label. An empty group clears it, so no attribute is emitted at all
// rather than an empty one - a deployment that does not partition by group produces exactly the
// series it did before.
func setGroup(value string) {
	if value == "" {
		group.Store(nil)

		return
	}

	kv := attribute.String(GroupAttribute, value)

	group.Store(&kv)
}
