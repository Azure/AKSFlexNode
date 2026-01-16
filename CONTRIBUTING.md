# Contributing to AKS Flex Node

Thank you for your interest in contributing to AKS Flex Node! This document provides guidelines and instructions for contributing to the project.

## Table of Contents

- [Code of Conduct](#code-of-conduct)
- [Getting Started](#getting-started)
- [Development Setup](#development-setup)
- [Making Changes](#making-changes)
- [Testing](#testing)
- [Code Quality](#code-quality)
- [Submitting Changes](#submitting-changes)
- [Release Process](#release-process)

## Code of Conduct

This project adheres to the [Microsoft Open Source Code of Conduct](https://opensource.microsoft.com/codeofconduct/). By participating, you are expected to uphold this code.

## Getting Started

1. Fork the repository on GitHub
2. Clone your fork locally:
   ```bash
   git clone https://github.com/YOUR-USERNAME/AKSFlexNode.git
   cd AKSFlexNode
   ```
3. Add the upstream repository:
   ```bash
   git remote add upstream https://github.com/Azure/AKSFlexNode.git
   ```

## Development Setup

### Prerequisites

- Go 1.24 or later
- Ubuntu 22.04 or 24.04 (for full testing)
- Make
- golangci-lint (for linting)

### Install Dependencies

```bash
# Verify Go installation
go version

# Install development dependencies
go mod download

# Install golangci-lint (if not already installed)
curl -sSfL https://raw.githubusercontent.com/golangci/golangci-lint/master/install.sh | sh -s -- -b $(go env GOPATH)/bin latest
```

## Making Changes

### Branch Naming

Use descriptive branch names:
- `feature/` - New features (e.g., `feature/add-networking`)
- `fix/` - Bug fixes (e.g., `fix/arc-registration`)
- `docs/` - Documentation updates (e.g., `docs/update-readme`)
- `refactor/` - Code refactoring (e.g., `refactor/bootstrap-logic`)

### Commit Messages

Follow conventional commit format:
```
<type>: <description>

[optional body]

[optional footer]
```

Types: `feat`, `fix`, `docs`, `style`, `refactor`, `test`, `chore`

Examples:
```
feat: add support for custom CNI plugins

fix: resolve arc registration timeout issue

docs: update installation instructions for Ubuntu 24.04
```

## Testing

### Running Tests

```bash
# Run all tests
make test

# Run tests with coverage
make test-coverage

# Run tests with race detector
make test-race
```

### Writing Tests

- Write tests for all new functionality
- Maintain or improve code coverage
- Use table-driven tests where appropriate
- Follow existing test patterns in the codebase

## Code Quality

### Formatting

```bash
# Format code
make fmt

# Format imports
make fmt-imports

# Format everything
make fmt-all
```

### Linting

```bash
# Run linter
make lint

# Run go vet
make vet
```

### Pre-submission Checklist

Before submitting your PR, ensure:

```bash
# Run all quality checks
make check

# This runs: fmt-all, vet, lint, and test
```

## Submitting Changes

1. **Create a Pull Request:**
   - Push your changes to your fork
   - Create a PR against the `main` branch
   - Fill out the PR template completely

2. **PR Requirements:**
   - All tests must pass
   - Code must be formatted and linted
   - PR must have a clear description
   - Link related issues

3. **Review Process:**
   - Maintainers will review your PR
   - Address any feedback
   - Once approved, a maintainer will merge

## Release Process

### For Maintainers

Creating a new release involves tagging a commit and triggering the release workflow.

#### Using the Release Script (Recommended)

```bash
# Create and push a release tag
./scripts/create-release.sh v0.0.4
```

The script will:
1. Validate the tag format
2. Check for a clean working tree
3. Create an annotated tag
4. Push the tag to origin
5. Trigger the release workflow automatically

#### Manual Release Process

If you prefer to create the release manually:

```bash
# Ensure you're on the main branch with latest changes
git checkout main
git pull origin main

# Create an annotated tag
git tag -a v0.0.4 -m "Release v0.0.4"

# Push the tag to trigger the release workflow
git push origin v0.0.4
```

#### What Happens Next

Once the tag is pushed:

1. **Automated Build:** GitHub Actions will build binaries for:
   - Linux AMD64
   - Linux ARM64

2. **Release Creation:** A GitHub release will be created with:
   - Auto-generated release notes
   - Binary artifacts (`.tar.gz` files)
   - SHA256 checksums

3. **Verification:** Check the release at:
   ```
   https://github.com/Azure/AKSFlexNode/releases/tag/v0.0.4
   ```

#### Release Workflow Details

The release is managed by `.github/workflows/release.yml` which:
- Builds cross-platform binaries
- Packages them with version information
- Generates checksums
- Creates a GitHub release with all artifacts

#### Monitoring the Release

Monitor the workflow at:
```
https://github.com/Azure/AKSFlexNode/actions
```

### Version Numbering

Follow [Semantic Versioning](https://semver.org/):
- **MAJOR** version (v1.0.0): Incompatible API changes
- **MINOR** version (v0.1.0): Add functionality (backwards-compatible)
- **PATCH** version (v0.0.1): Backwards-compatible bug fixes

### Pre-release Versions

For alpha/beta releases, append a suffix:
```bash
git tag -a v0.1.0-alpha.1 -m "Release v0.1.0-alpha.1"
git push origin v0.1.0-alpha.1
```

## Questions?

- Open an issue for bugs or feature requests
- Start a discussion for questions or ideas
- Contact maintainers for sensitive issues

## License

By contributing, you agree that your contributions will be licensed under the project's MIT License.

---

Thank you for contributing to AKS Flex Node! 🚀
