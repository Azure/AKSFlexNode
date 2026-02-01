# Contributing to AKS Flex Node

Thank you for your interest in contributing to AKS Flex Node! This guide covers everything you need to know about developing, testing, and contributing to the project.

## Table of Contents

- [Development Setup](#development-setup)
- [Building from Source](#building-from-source)
- [Testing](#testing)
  - [Unit Tests](#unit-tests)
  - [E2E Testing Infrastructure](#e2e-testing-infrastructure)
- [Code Quality](#code-quality)
- [GitHub Actions Workflows](#github-actions-workflows)
- [Pull Request Process](#pull-request-process)

---

## Development Setup

### Prerequisites

- Go 1.24+
- Ubuntu 22.04 LTS (for testing)
- Azure CLI (for E2E testing)
- Make

### Clone and Build

```bash
# Clone the repository
git clone https://github.com/your-org/AKSFlexNode.git
cd AKSFlexNode

# Build the application
make build

# Run tests
make test

# View all available commands
make help
```

---

## Building from Source

### Build Commands

```bash
# Build for current platform
make build

# Build for specific platforms
make build-linux-amd64
make build-linux-arm64

# Build all platforms
make build-all

# Clean build artifacts
make clean
```

### Version Information

Version info is injected at build time:

```bash
# Build with version info
VERSION=v1.0.0 make build

# View version
./aks-flex-node version
```

---

## Testing

### Unit Tests

#### Running Tests

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

#### Test Coverage

The project enforces a minimum test coverage threshold of **30%**. To view detailed coverage:

```bash
make test-coverage
# Opens coverage.html showing line-by-line coverage
```

Coverage reports are uploaded as artifacts in GitHub Actions runs for review.

#### Writing Tests

**Test File Conventions:**
- Test files end with `_test.go`
- Place tests in the same package as the code being tested
- Use table-driven tests for multiple test cases
- Use subtests with `t.Run()` for better organization

**Example Test Structure:**

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

**Testing Best Practices:**

1. **Test behavior, not implementation** - Focus on what the code does, not how
2. **Use meaningful test names** - Describe what is being tested
3. **Keep tests simple** - Each test should verify one thing
4. **Mock external dependencies** - Use interfaces for testability
5. **Test edge cases** - Include boundary conditions and error cases
6. **Use test fixtures** - Keep test data organized and reusable

---

### E2E Testing Infrastructure

The project uses self-hosted GitHub Actions runners for true End-to-End testing.

#### 🎯 What E2E Tests Do

The E2E workflow tests the **complete user journey**:

1. ✅ Creates fresh AKS cluster
2. ✅ Creates test VM
3. ✅ Runs `aks-flex-node bootstrap` on the VM
4. ✅ Verifies Arc registration
5. ✅ Verifies VM joined AKS cluster as flex node
6. ✅ Cleans up everything (cluster + VM + Arc)

**Duration:** ~20-25 minutes per test
**Cost:** ~$0.30 per test run
**Key Benefit:** Tests create/destroy fresh clusters - no persistent infrastructure needed!

#### 🚀 Quick Setup (3 Steps)

**Step 1: Configure .env**

```bash
cp .env.example .env
nano .env
```

Fill in:
```bash
AZURE_SUBSCRIPTION_ID=<your-subscription-id>
AZURE_TENANT_ID=<your-tenant-id>
GITHUB_ORG=<your-github-org>

# E2E test configuration (where tests create/delete clusters & VMs)
E2E_RESOURCE_GROUP=rg-aksflexnode-e2e-tests
E2E_LOCATION=westus2

# Runner VM configuration (separate from test resources)
RUNNER_RESOURCE_GROUP=rg-aksflexnode-e2e-runner
RUNNER_LOCATION=westus2
RUNNER_VM_NAME=vm-e2e-runner
```

**Note:** No existing AKS cluster needed - tests create their own!

**Step 2: Create & Register Runner**

```bash
# Login to Azure
az login

# Create self-hosted runner (~10 min)
./scripts/setup/setup-runner.sh

# Get token from: https://github.com/YOUR_ORG/AKSFlexNode/settings/actions/runners/new
# Register runner with GitHub
./scripts/setup/register-runner.sh <TOKEN>
```

Verify: https://github.com/YOUR_ORG/AKSFlexNode/settings/actions/runners
Should see: `aksflexnode-e2e-runner` (Idle) 🟢

**Step 3: Add GitHub Secrets**

Add these 4 secrets to GitHub:

**Configuration:** Get from your Azure account
```
E2E_RESOURCE_GROUP       - Resource group for test VMs (e.g., rg-aksflexnode-e2e-tests)
E2E_LOCATION             - Azure region (e.g., westus2)
AZURE_SUBSCRIPTION_ID    - Your Azure subscription ID
AZURE_TENANT_ID          - Your Azure tenant ID
```

Go to: https://github.com/YOUR_ORG/AKSFlexNode/settings/secrets/actions

**Note:** Test VMs use their own Managed Identity - no Service Principal needed!

#### 🧪 Running E2E Tests

```bash
# Manual trigger
# Go to: Actions → E2E Tests → Run workflow

# Or automatically on release
git tag v1.0.0
git push --tags
```

**What it does:**
1. Builds aks-flex-node binary
2. Creates fresh AKS cluster
3. Creates Ubuntu 22.04 test VM
4. Runs bootstrap on test VM
5. Verifies Arc registration
6. Verifies node joins AKS cluster
7. Cleans up cluster + VM + Arc

**Time:** ~20-25 minutes per test

#### 🔧 Managing E2E Infrastructure

**Start/Stop Runner VM** (save costs)

```bash
# Stop (saves ~$40/month)
az vm deallocate -g rg-aksflexnode-e2e-runner -n vm-e2e-runner

# Start (takes 2-3 min)
az vm start -g rg-aksflexnode-e2e-runner -n vm-e2e-runner
```

**Start/Stop AKS Cluster**

```bash
./scripts/setup/stop-aks-cluster.sh  # Save costs
./scripts/setup/start-aks-cluster.sh  # Resume
```

**Check Runner Status**

```bash
source .env
ssh azureuser@${RUNNER_PUBLIC_IP} "sudo systemctl status actions.runner.*"
```

#### 💰 E2E Infrastructure Cost

| Component | Cost/Month | Can Stop? |
|-----------|------------|-----------|
| Runner VM | ~$60 | Yes (~$0 when deallocated) |
| E2E test run | ~$0.30/test | Auto-deleted |
| **Total (10 tests/month)** | **~$3** | - |
| **Total (100 tests/month)** | **~$30** | - |

*Note: E2E tests create/delete clusters automatically - no persistent cluster costs!*

#### 🐛 E2E Troubleshooting

**Runner not showing in GitHub:**
- Check service: `ssh runner "sudo systemctl status actions.runner.*"`
- Check logs: `ssh runner "sudo journalctl -u actions.runner.* -n 50"`
- Re-register: Get new token and run `register-runner.sh`

**E2E test fails:**
- Check logs in workflow artifacts
- SSH to test VM before cleanup: Use `skip_cleanup: true` input
- Check Arc agent: `sudo azcmagent show`

**Permission denied:**
- Verify MSI roles: `az role assignment list --assignee <MSI-ID>`
- Wait 5-10 min for role propagation

---

## Code Quality

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

### Pre-commit Checklist

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

---

## GitHub Actions Workflows

The project has three GitHub Actions workflows:

### 📊 Workflow Design

| Workflow | Runs On | Purpose | Azure Access |
|----------|---------|---------|--------------|
| `pr-checks.yml` | GitHub runners | Build, test, lint | No |
| `release.yml` | GitHub runners | Build releases | No |
| `e2e-tests.yml` | Self-hosted | E2E testing | Yes (MSI) |

**Key:** Only E2E workflow runs on self-hosted runner!

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

### E2E Tests Workflow (`.github/workflows/e2e-tests.yml`)

Runs on self-hosted runner with Managed Identity authentication. See [E2E Testing Infrastructure](#e2e-testing-infrastructure) section above.

### Release Workflow (`.github/workflows/release.yml`)

Automatically builds and publishes releases when tags are pushed.

---

## Pull Request Process

### Pull Request Flow

1. Fork the repository
2. Create your feature branch (`git checkout -b feature/amazing-feature`)
3. Make your changes
4. Run pre-commit checks: `make verify && make check && make build-all`
5. Commit your changes (`git commit -m 'Add amazing feature'`)
6. Push to the branch (`git push origin feature/amazing-feature`)
7. Open a Pull Request

### PR Requirements

Before your PR can be merged:

1. ✅ All CI checks must pass (Build, Test, Lint, Security, Code Quality)
2. ✅ Code coverage must not drop below 30%
3. ✅ Code must be properly formatted (`make fmt-all`)
4. ✅ No linter warnings or errors
5. ✅ PR description clearly explains the changes
6. ✅ Commits follow conventional commit format (recommended)
7. ✅ All conversations must be resolved

### Branch Protection

Recommended branch protection rules for `main` and `dev`:

- ✅ Require pull request reviews before merging
- ✅ Require status checks to pass before merging
  - Build (Go 1.24)
  - Test
  - Lint
  - Security
  - Code Quality
- ✅ Require branches to be up to date before merging
- ✅ Require conversation resolution before merging

---

## Troubleshooting

### Test Failures

If tests fail in CI but pass locally:

1. Check Go version matches CI (1.24)
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

---

## Additional Documentation

- **Architecture:** [ARCHITECTURE.md](ARCHITECTURE.md) - System architecture and design
- **Development Guide:** [CLAUDE.md](CLAUDE.md) - Guide for Claude Code development

---

## Questions or Issues?

- Report issues: [GitHub Issues](https://github.com/your-org/AKSFlexNode/issues)
- Discussion: [GitHub Discussions](https://github.com/your-org/AKSFlexNode/discussions)

---

**Thank you for contributing to AKS Flex Node!** 🚀
