# AKS FlexNode Agent

Go agent that extends AKS to non-Azure VMs, transforming Ubuntu machines into semi-managed AKS worker nodes.

## Coding Conventions

- **Language**: Go 1.25+. Standard `gofmt`/`goimports` formatting.
- **Linting**: `golangci-lint` with errcheck (type assertions checked), gosec, govet, ineffassign, staticcheck, unused. Run `make lint`.
- **Quality gate**: `make check` runs formatting, vetting, linting, and tests in sequence.
- **Naming**: packages lowercase; files snake_case; unexported types/vars camelCase; exported PascalCase.
- **Errors**: wrap with context via `fmt.Errorf("context: %w", err)`. Use gRPC status codes in `components/`. Never panic in runtime logic.
- **Logging**: logrus with context propagation (`pkg/logger`). Log at appropriate levels — debug for verbose, info for state changes, error for failures.
- **Comments**: explain *why*, not *what*. Reference upstream docs with URLs where relevant.
- **Protobuf**: edition 2024, opaque API with builder pattern. Run `make generate` to regenerate `.pb.go` files.
- **Sensitive data**: never log or return tokens/secrets. Use `Redact()` in components; avoid committing `.env` or credential files.

## Testing

### Conventions

- **Table-driven tests** with named subtests (`t.Run(name, ...)`).
- **Parallel execution**: call `t.Parallel()` in both top-level and sub-tests.
- **Filesystem isolation**: use `t.TempDir()` for any file I/O.
- **Dependency injection**: accept interfaces on structs; use mock implementations in tests (see `pkg/components/arc/` for example).
- **Idempotency**: explicitly test that operations are safe to re-run.
- **No external test frameworks**: use the standard `testing` package only.

### Makefile Targets

Run `make help` to discover all available build, test, and lint targets.

## Key Design Patterns

1. **Idempotent reconciliation** — compare current state with desired state; only mutate when they differ.
2. **Bootstrapper executor** — sequential step execution with fail-fast (bootstrap) or best-effort (unbootstrap) semantics.
3. **Action hub** — single gRPC endpoint dispatches to versioned component handlers by protobuf type URL.
4. **Interface-driven** — depend on interfaces, not concrete types, to enable testing and extensibility.
