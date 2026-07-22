# hippocampus-mcp

An [MCP](https://modelcontextprotocol.io) server that gives an LLM host (Claude Desktop, Claude
Code, or any other MCP client) tools for storing and recalling memories in a running Hippocampus
instance.

It is a thin **bridge**: every tool call is turned into a gRPC request against the Hippocampus
service named by `--address`, so it holds no state and can be spawned, killed, and restarted freely
by the host.

```sh
go build -o hippocampus-mcp ./cmd/hippocampus-mcp
hippocampus-mcp --address localhost:50051
```

The full guide — tool reference, stdio/HTTP transports, auth/TLS, and Claude Desktop / Claude Code
configuration snippets — lives in **[docs/mcp.md](../../docs/mcp.md)**.
