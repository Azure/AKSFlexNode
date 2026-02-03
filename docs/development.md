# Development Guide

This guide covers building, testing, and contributing to AKS Flex Node.

## Building from Source

For developers who want to build from source:

```bash
# Build the application
make build

# Build for all platforms (linux/amd64, linux/arm64)
make build-all

# Create release archives
make package-all

# Run tests
make test

# Run tests with coverage
make test-coverage
```

For a complete list of build targets, run `make help`.

## Prerequisites

- **Operating System:** Ubuntu 22.04 LTS, 24.04 LTS, or compatible Linux distribution
- **Architecture:** x86_64 (amd64) or arm64
- **Go:** Version 1.24 or higher
- **Make:** GNU Make
- **Git:** For version control

## Development Workflow

### Code Quality Checks

```bash
# Run all checks (format, vet, lint, test)
make check

# Format code and organize imports
make fmt-all

# Run linter
make lint

# Run go vet
make vet

# Verify and tidy dependencies
make verify
```

### Testing

```bash
# Run all tests
make test

# Run tests with race detection
make test-race

# Run tests with coverage report (opens coverage.html)
make test-coverage

# Run specific package tests
go test ./pkg/config/
go test ./pkg/logger/
```

### Pre-commit Workflow

Before committing changes, ensure all checks pass:

```bash
make verify && make check && make build-all
```

## Testing and CI/CD

### Overview

The project uses GitHub Actions for automated testing on pull requests and pushes to main/dev branches. The testing infrastructure includes:

- **Build verification** across Go 1.24
- **Unit tests** with race detection and coverage reporting
- **Code quality checks** with multiple linters
- **Security scanning** with gosec
- **Dependency review** for vulnerabilities

### GitHub Actions Workflows

The PR checks workflow (`.github/workflows/pr-checks.yml`) runs automatically on:
- Pull requests to `main` or `dev` branches
- Direct pushes to `main` or `dev` branches

**Jobs:**

1. **Build** - Verifies the project builds successfully
   - Tests on Go 1.24
   - Builds for current platform and all supported platforms (linux/amd64, linux/arm64)

2. **Test** - Runs the test suite
   - Executes all tests with race detection
   - Generates coverage report
   - Reports coverage percentage (warns if below 30% but doesn't fail)

3. **Lint** - Runs golangci-lint with comprehensive checks
   - Uses `.golangci.yml` configuration
   - Checks code quality and common issues

4. **Security** - Scans for security vulnerabilities
   - Runs gosec security scanner
   - Uploads results to GitHub Security tab

5. **Code Quality** - Additional quality checks
   - Verifies code formatting with `gofmt`
   - Verifies import formatting with `goimports`
   - Runs `go vet` for correctness
   - Runs `staticcheck` for additional static analysis

6. **Dependency Review** - Reviews dependencies for security issues
   - Only runs on pull requests
   - Fails on moderate or higher severity vulnerabilities

### Installing golangci-lint

The project uses golangci-lint v2. If you don't have it installed:

```bash
# Linux/macOS (installs latest version)
curl -sSfL https://raw.githubusercontent.com/golangci/golangci-lint/master/install.sh | sh -s -- -b $(go env GOPATH)/bin

# macOS with Homebrew
brew install golangci-lint

# Or use Go install
go install github.com/golangci/golangci-lint/cmd/golangci-lint@latest
```

### Linter Configuration

The project uses `.golangci.yml` (v2 format) for linter configuration with the following enabled checks:

**Enabled Linters:**
- errcheck - Checks for unchecked errors (with `check-blank: false` to allow `_ =` in defer)
- govet - Reports suspicious constructs
- ineffassign - Detects ineffectual assignments
- staticcheck - Advanced static analysis (includes gosimple checks)
- unused - Finds unused code

**Exclusions:**
- Test files (`_test.go`) are excluded from errcheck to allow testing error conditions

### Test Coverage

The project enforces a minimum test coverage threshold of **30%**. To view detailed coverage:

```bash
make test-coverage
# Opens coverage.html showing line-by-line coverage
```

Coverage reports are uploaded as artifacts in GitHub Actions runs for review.

### Writing Tests

#### Test File Conventions

- Test files end with `_test.go`
- Place tests in the same package as the code being tested
- Use table-driven tests for multiple test cases
- Use subtests with `t.Run()` for better organization

#### Example Test Structure

```go
func TestFunctionName(t *testing.T) {
    tests := []struct {
        name    string
        input   string
        want    string
        wantErr bool
    }{
        {
            name:    "valid input",
            input:   "test",
            want:    "expected",
            wantErr: false,
        },
        // more test cases...
    }

    for _, tt := range tests {
        t.Run(tt.name, func(t *testing.T) {
            got, err := FunctionName(tt.input)
            if (err != nil) != tt.wantErr {
                t.Errorf("FunctionName() error = %v, wantErr %v", err, tt.wantErr)
                return
            }
            if got != tt.want {
                t.Errorf("FunctionName() = %v, want %v", got, tt.want)
            }
        })
    }
}
```

#### Testing Best Practices

1. **Test behavior, not implementation** - Focus on what the code does, not how
2. **Use meaningful test names** - Describe what is being tested
3. **Keep tests simple** - Each test should verify one thing
4. **Mock external dependencies** - Use interfaces for testability
5. **Test edge cases** - Include boundary conditions and error cases
6. **Use test fixtures** - Keep test data organized and reusable

### Troubleshooting

#### Test Failures

If tests fail in CI but pass locally:

1. Check Go version matches CI (1.24)
2. Run with race detector: `make test-race`
3. Check for environment-specific issues
4. Ensure dependencies are up to date: `make verify`

#### Linter Failures

If linter fails in CI but passes locally:

1. Ensure golangci-lint version matches CI (latest)
2. Run: `make lint`
3. Check `.golangci.yml` for configuration
4. Some issues may be platform-specific

#### Coverage Below Threshold

If coverage drops below 30%:

1. Add tests for new code
2. Focus on critical paths first
3. Review `coverage.html` for uncovered lines
4. Consider raising threshold as coverage improves

## Project Structure

```
AKSFlexNode/
├── cmd/                     # Command-line interface
├── pkg/
│   ├── auth/               # Azure authentication
│   ├── azure/              # Azure API interactions
│   ├── bootstrapper/       # Bootstrap orchestration
│   ├── components/         # Component installers
│   ├── config/             # Configuration management
│   ├── logger/             # Logging infrastructure
│   └── utils/              # Utility functions
├── scripts/                # Installation scripts
├── docs/                   # Documentation
├── Makefile               # Build and test targets
└── go.mod                 # Go module definition
```

## Code Style and Conventions

- Follow standard Go conventions and idioms
- Use `gofmt` for code formatting
- Pass `golangci-lint` checks
- Write meaningful commit messages
- Add tests for new functionality
- Update documentation as needed

## Adding New Features

When adding new features:

1. Create a feature branch from `main`
2. Implement your changes with appropriate tests
3. Ensure all checks pass: `make verify && make check`
4. Update documentation if needed
5. Submit a pull request with a clear description

### Adding a New Bootstrap Component

If adding a new component to the bootstrap process:

1. Create a new directory in `pkg/components/`
2. Implement the `Executor` interface (Install/Uninstall methods)
3. Add the component to the bootstrap sequence in `pkg/bootstrapper/bootstrapper.go`
4. Consider dependencies and execution order
5. Add appropriate tests

## Contributing

We welcome contributions! Here's how to get started:

1. Fork the repository
2. Create your feature branch (`git checkout -b feature/amazing-feature`)
3. Commit your changes (`git commit -m 'Add amazing feature'`)
4. Push to the branch (`git push origin feature/amazing-feature`)
5. Open a Pull Request

### Pull Request Guidelines

- Provide a clear description of the changes
- Reference any related issues
- Ensure all CI checks pass
- Update documentation for user-facing changes
- Add tests for new functionality
- Follow the existing code style

### Pull Request Flow

1. Developer opens PR
2. GitHub Actions automatically runs all checks
3. All jobs must pass (green) before merge
4. Reviews are conducted
5. PR is merged to target branch

### Branch Protection

Recommended branch protection rules for `main` and `dev`:

- Require pull request reviews before merging
- Require status checks to pass before merging (Build, Test, Lint, Security, Code Quality)
- Require branches to be up to date before merging
- Require conversation resolution before merging

## License

This project is licensed under the MIT License - see the [LICENSE](../LICENSE.MD) file for details.

## Getting Help

- **Issues:** [GitHub Issues](https://github.com/Azure/AKSFlexNode/issues)
- **Discussions:** [GitHub Discussions](https://github.com/Azure/AKSFlexNode/discussions)
- **Documentation:** See the [docs/](.) directory

## Additional Resources

- [Usage Guide](usage.md) - Installation and configuration
- [Design Documentation](design.md) - Complete system design and technical architecture
- [CLAUDE.md](../CLAUDE.md) - Development guidance for Claude Code
