# hippo

`hippo` is a command-line client for a running [Hippocampus](../../README.md) service. It exposes
the full RPC surface as noun-verb subcommands and can talk to the service over **either** transport:
native gRPC (the default) or the JSON/HTTP `/v1` gateway (`--transport http`).

It is a thin client — it holds no state, dials the service named by `--address`, and turns each
command into one RPC. Both transports share one client interface, so every command behaves
identically whichever is selected; what a token is actually allowed to do is enforced by the
service's auth tiers, not by this tool.

This client is its own Go module, so build it from this directory:

```sh
go build -o hippo .

# gRPC (default)
./hippo --address localhost:50051 whoami
./hippo memory store --body "remember this" --significance 6 --group svc-a

# HTTP /v1 gateway
./hippo --transport http --address localhost:8080 memory list --group svc-a --limit 20
```

Every global flag may also be supplied via a `HIPPOCAMPUS_<FLAG>` environment variable (for example
`HIPPOCAMPUS_TOKEN` for the bearer token). Run `hippo --help` for the command list and global
flags, or `hippo <command> --help` for a single command.

Shell completion (bash/zsh/fish) is built in and stays in sync with the commands:

```sh
source <(hippo completion bash)   # or: hippo completion fish | source
```

The full guide — command reference, the gRPC/HTTP transports, output formats, and auth/TLS — lives
in **[docs/cli.md](../../docs/cli.md)**.
