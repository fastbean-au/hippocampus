// Package hippocampus implements the Hippocampus gRPC service: a memory store with finite capacity
// that forgets what has become insignificant.
//
// Server is the implementation of contract.HippocampusServer. It stores memories and events, and runs
// the sleep cycle (sleep.go) that consolidates - deletes - whatever has decayed below the deletion
// threshold, evicts by value when a capacity target is exceeded, and compacts the store. Recalling a
// memory reinforces it: the decay clock resets and its effective significance rises.
//
// Around that core sit the forgetting-transparency RPCs (PreviewConsolidation, ExplainConsolidation,
// GetConsolidationStatus and the forgotten log), the transfer and archive surface, outbound
// callbacks, content search over the search package, optional summarisation over the summarise
// package, and the deployment topology view.
//
// Group scoping is enforced here rather than in the auth package: scope.go declares, for every RPC,
// how a caller's group scope reaches the store, and a test refuses an RPC that declares nothing.
//
// The server reads no configuration of its own: New takes a Config, which main.go builds. Storage is
// reached through db.Store.
package hippocampus
