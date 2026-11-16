package contract

// RPCNames is every RPC the service declares, unary and streaming alike, by bare method name.
//
// grpc-go lists the two in separate fields of the service descriptor - Methods and Streams - so a
// drift guard walking Methods alone passes an RPC that streams without ever seeing it. Watch is the
// first such RPC, and every table keyed by RPC name (the tiers, the scope modes, the isolation
// tests) walks this instead.
func RPCNames() []string {
	names := make([]string, 0, len(Hippocampus_ServiceDesc.Methods)+len(Hippocampus_ServiceDesc.Streams))

	for _, method := range Hippocampus_ServiceDesc.Methods {
		names = append(names, method.MethodName)
	}

	for _, stream := range Hippocampus_ServiceDesc.Streams {
		names = append(names, stream.StreamName)
	}

	return names
}
