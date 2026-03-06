# Components Module

## Design Overview

Action-based plugin architecture for AKS node provisioning. Each component models a discrete node operation (download binaries, configure services, join cluster) as a **protobuf Action** with `Metadata`, `Spec` (desired state), and `Status` (result).

### Architecture

```
Caller → Hub (gRPC server) → component handler
```

- **Hub** (`services/actions/hub.go`): Central dispatcher mapping protobuf type URLs to `Server` implementations.
- **Registration**: Each versioned package registers via `init()` in `exports.go`; top-level `exports.go` triggers all registrations via blank imports.
- **In-memory gRPC** (`services/inmem/`): Same-process action invocation without network overhead.

### Two-Layer Package Structure

- **Parent package** (e.g. `cni/`): Protobuf definitions, defaulting, validation, redaction — the data model layer.
- **Versioned sub-package** (e.g. `cni/v20260301/`): Concrete implementation of actions.

Components are independent of each other. Shared utilities come from `pkg/`.

## Key Principles

1. **Idempotency** — Every action is safe to re-run. Compare current state with desired state; only write/restart when something changed.
2. **Declarative Spec/Status** — Actions declare desired state in `Spec` and report actual state in `Status`.
3. **Default → Validate pipeline** — Use `api.DefaultAndValidate[M]()` for consistent processing.
4. **Security** — Sensitive fields (tokens, secrets) must be redacted via `Redact()` before leaving the Hub.
5. **Separation of concerns** — Data models (proto + defaulting + redaction) in parent package; logic in versioned sub-package.

## Coding Conventions

- **Go** with `gofmt`/`goimports`. Lint via `golangci-lint` (errcheck, gosec, govet, staticcheck, unused). Use `make fmt`, `make lint`, and `make check` to run these.
- **Protobuf edition 2024**, opaque API with builder pattern. Run `make generate` to regenerate `.pb.go` files from `.proto` definitions.
- **Naming**: packages lowercase (`cni`, `kubelet`); files snake_case (`kubelet_config.go`); unexported action types camelCase (`downloadCNIBinariesAction`); factory functions `new*Action()`.
- **Compile-time interface checks**: `var _ actions.Server = (*myAction)(nil)`.
- **Errors**: Use gRPC status codes (`status.Errorf(codes.Internal, ...)`). Wrap with `fmt.Errorf("context: %w", err)`.
- **Embedded assets**: `//go:embed assets/*` with `text/template` for systemd units and config files.
- **Comments**: Explain *why*, not just *what*. Include upstream reference URLs where relevant.

## Adding a New Component

1. Create `components/<name>/` with `action.proto`, `defaulting.go`, `redact.go`.
2. Create `components/<name>/v<component version>/` with `exports.go` (init registration) and action implementation files.
3. Add blank import in `components/exports.go`.
4. Implement `actions.Server` interface; assert at compile time.
5. Place templates in `assets/` subdirectory; embed with `//go:embed`.

## Testing

- Table-driven tests, parallel execution (`t.Parallel()`), `t.TempDir()` for filesystem isolation.
- Test idempotency explicitly.
- Action structs accept interfaces for dependencies to enable test doubles.
- Run: `make test`, `make test-coverage`, `make test-race`.
