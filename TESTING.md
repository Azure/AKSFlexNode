# Testing and CI/CD Guide

This document describes the testing infrastructure and continuous integration/continuous deployment (CI/CD) workflows for AKS Flex Node.

## Overview

The project uses GitHub Actions for automated testing on pull requests and pushes to main/dev branches. The testing infrastructure includes:

- **Build verification** across Go 1.24
- **Unit tests** with race detection and coverage reporting
- **Code quality checks** with multiple linters
- **Security scanning** with gosec
- **Dependency review** for vulnerabilities

## GitHub Actions Workflows

### PR Checks Workflow (`.github/workflows/pr-checks.yml`)

This workflow runs automatically on:
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

## Local Development

### Running Tests

```bash
# Run all tests
make test

# Run tests with coverage report
make test-coverage
# Opens coverage.html in browser after generation

# Run tests with race detector
make test-race

# Run specific package tests
go test ./pkg/config/
go test ./pkg/logger/
```

### Code Quality Checks

```bash
# Format code
make fmt

# Format imports
make fmt-imports

# Format both code and imports
make fmt-all

# Run go vet
make vet

# Run linter
make lint

# Run all quality checks (fmt-all + vet + lint + test)
make check

# Verify and tidy dependencies
make verify
```

### Installing golangci-lint

If you don't have golangci-lint installed:

```bash
# Linux/macOS
curl -sSfL https://raw.githubusercontent.com/golangci/golangci-lint/master/install.sh | sh -s -- -b $(go env GOPATH)/bin

# macOS with Homebrew
brew install golangci-lint

# Or use Go install
go install github.com/golangci/golangci-lint/cmd/golangci-lint@latest
```

## Linter Configuration

The project uses `.golangci.yml` for linter configuration with the following enabled checks:

**Default Linters:**
- errcheck - Checks for unchecked errors
- gosimple - Suggests code simplifications
- govet - Reports suspicious constructs
- ineffassign - Detects ineffectual assignments
- staticcheck - Advanced static analysis
- unused - Finds unused code

**Additional Linters:**
- bodyclose - Checks HTTP response body is closed
- dupl - Finds duplicate code
- gocyclo - Checks cyclomatic complexity
- gofmt - Validates code formatting
- goimports - Checks import formatting
- gosec - Security vulnerability scanner
- misspell - Finds commonly misspelled words
- revive - Fast, configurable, extensible Go linter

**Exclusions:**
- G204 (subprocess with variable) - Expected for this project
- G304 (file path taint) - Expected for config loading

## Test Coverage

The project enforces a minimum test coverage threshold of **30%**. To view detailed coverage:

```bash
make test-coverage
# Opens coverage.html showing line-by-line coverage
```

Coverage reports are uploaded as artifacts in GitHub Actions runs for review.

## Pre-commit Checklist

Before submitting a PR, run:

```bash
# Run all checks
make check

# Verify build works
make build-all

# Check dependencies
make verify
```

Or run the full suite:

```bash
make verify && make check && make build-all
```

## Continuous Integration

### Pull Request Flow

1. Developer opens PR
2. GitHub Actions automatically runs all checks
3. All jobs must pass (green) before merge
4. Reviews are conducted
5. PR is merged to target branch

### Branch Protection

Recommended branch protection rules for `main` and `dev`:

- ✅ Require pull request reviews before merging
- ✅ Require status checks to pass before merging
  - Build (Go 1.23)
  - Build (Go 1.24)
  - Test
  - Lint
  - Security
  - Code Quality
- ✅ Require branches to be up to date before merging
- ✅ Require conversation resolution before merging

## Writing Tests

### Test File Conventions

- Test files end with `_test.go`
- Place tests in the same package as the code being tested
- Use table-driven tests for multiple test cases
- Use subtests with `t.Run()` for better organization

### Example Test Structure

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

### Testing Best Practices

1. **Test behavior, not implementation** - Focus on what the code does, not how
2. **Use meaningful test names** - Describe what is being tested
3. **Keep tests simple** - Each test should verify one thing
4. **Mock external dependencies** - Use interfaces for testability
5. **Test edge cases** - Include boundary conditions and error cases
6. **Use test fixtures** - Keep test data organized and reusable

## Troubleshooting

### Test Failures

If tests fail in CI but pass locally:

1. Check Go version matches CI (1.23)
2. Run with race detector: `make test-race`
3. Check for environment-specific issues
4. Ensure dependencies are up to date: `make verify`

### Linter Failures

If linter fails in CI but passes locally:

1. Ensure golangci-lint version matches CI (latest)
2. Run: `make lint`
3. Check `.golangci.yml` for configuration
4. Some issues may be platform-specific

### Coverage Below Threshold

If coverage drops below 30%:

1. Add tests for new code
2. Focus on critical paths first
3. Review `coverage.html` for uncovered lines
4. Consider raising threshold as coverage improves

## Future Enhancements

Potential improvements to the testing infrastructure:

- Integration tests with actual Arc registration (requires Azure credentials)
- Performance benchmarks
- E2E tests in containerized environment
- Automated release notes generation
- Code coverage trending over time
