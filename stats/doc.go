// Package stats logs the store's event and memory counts on an interval (stats.intervalSeconds) and
// publishes them as gauges. The log line and the gauges share one cached count, so the full-table
// counts run at most once per interval however often metrics are exported.
package stats
