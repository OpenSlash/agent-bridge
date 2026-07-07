# Agent Bridge

Agent Bridge provides shared protocol and runtime packages for proxying Codex
and Claude CLI sessions over remote transports.

## Packages

- `protocol` - wire message types and payload contracts.
- `runtime` - session runtime, process bridge, attached mode, permissions, and
  resource helpers.

## Development

```bash
go test ./...
```

## Module

```go
module github.com/OpenSlash/agent-bridge
```

## License

Agent Bridge is licensed under the Apache License, Version 2.0. See `LICENSE`.
