# Examples

Three small programs that do the same thing (store a memory, find it, recall it, ask where it
stands, delete it) through each way in:

| Example                                                   | Transport          | Run it                                                      |
| --------------------------------------------------------- | ------------------ | ----------------------------------------------------------- |
| [`go/quickstart`](go/quickstart/main.go)                  | gRPC, via `dial`   | `go run ./examples/go/quickstart --address localhost:50051` |
| [`python/agent_loop.py`](python/agent_loop.py)            | gRPC, asyncio      | `python examples/python/agent_loop.py`                      |
| [`curl/quickstart.sh`](curl/quickstart.sh)                | HTTP/JSON gateway  | `examples/curl/quickstart.sh`                               |

The Python one is shaped like an agent rather than a tour. Each turn recalls what is relevant (a
reinforcing search, so what keeps proving useful keeps surviving) and stores the turn under the
conversation's event.

All three take a bearer token (`--token`, or `HIPPOCAMPUS_TOKEN`) for a service with auth on.

They are **contract smoke tests** as well as documentation. CI runs the Go and curl examples against
the compose stack, and runs the Python loop against the client's fake service, so an example that no
longer matches the contract fails the build rather than the next person who copies it.
