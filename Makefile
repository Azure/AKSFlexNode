# AKS FlexNode Makefile
BUILD_DATE := $(shell date -u +"%Y-%m-%dT%H:%M:%SZ")
GIT_COMMIT := $(shell git rev-parse --short HEAD 2>/dev/null || echo "unknown")
VERSION := $(shell git describe --tags --always --dirty 2>/dev/null || echo "dev")

# Build flags to inject version information
LDFLAGS := -X main.Version=$(VERSION) -X main.GitCommit=$(GIT_COMMIT) -X main.BuildTime=$(BUILD_DATE) -w -s

# Default build for current platform
.PHONY: build
build:
	@echo "Building for current platform..."
	@go build -ldflags "$(LDFLAGS)" -o aks-flex-node .

# Cross-platform builds for supported architectures
.PHONY: build-linux-amd64
build-linux-amd64:
	@echo "Building for Linux AMD64..."
	@GOOS=linux GOARCH=amd64 go build -ldflags "$(LDFLAGS)" -o aks-flex-node-linux-amd64 .

.PHONY: build-linux-arm64
build-linux-arm64:
	@echo "Building for Linux ARM64..."
	@GOOS=linux GOARCH=arm64 go build -ldflags "$(LDFLAGS)" -o aks-flex-node-linux-arm64 .

# Build all supported platforms
.PHONY: build-all
build-all: build-linux-amd64 build-linux-arm64
	@echo "Built binaries for all supported platforms"

# Create release archives
.PHONY: package-linux-amd64
package-linux-amd64: build-linux-amd64
	@echo "Packaging Linux AMD64 binary..."
	@tar -czf aks-flex-node-linux-amd64.tar.gz aks-flex-node-linux-amd64

.PHONY: package-linux-arm64
package-linux-arm64: build-linux-arm64
	@echo "Packaging Linux ARM64 binary..."
	@tar -czf aks-flex-node-linux-arm64.tar.gz aks-flex-node-linux-arm64

# Package all supported platforms
.PHONY: package-all
package-all: package-linux-amd64 package-linux-arm64
	@echo "Packaged all supported platforms"
	@ls -la *.tar.gz

# Testing and quality checks
.PHONY: test
test:
	@echo "Running tests..."
	@go test -v ./...

.PHONY: test-coverage
test-coverage:
	@echo "Running tests with coverage..."
	@go test -v -race -coverprofile=coverage.out -covermode=atomic ./...
	@go tool cover -html=coverage.out -o coverage.html
	@echo "Coverage report generated: coverage.html"

.PHONY: test-race
test-race:
	@echo "Running tests with race detector..."
	@go test -race ./...

.PHONY: lint
lint:
	@echo "Running golangci-lint..."
	@which golangci-lint > /dev/null || (echo "golangci-lint not installed. Install from https://golangci-lint.run/usage/install/" && exit 1)
	@golangci-lint run --timeout=5m

.PHONY: fmt
fmt:
	@echo "Formatting code..."
	@gofmt -s -w .
	@echo "Code formatted"

.PHONY: fmt-imports
fmt-imports:
	@echo "Formatting imports..."
	@which goimports > /dev/null || (echo "goimports not installed. Installing..." && go install golang.org/x/tools/cmd/goimports@latest)
	@goimports -w .
	@echo "Imports formatted"

.PHONY: fmt-all
fmt-all: fmt fmt-imports
	@echo "All formatting complete"

.PHONY: vet
vet:
	@echo "Running go vet..."
	@go vet ./...

.PHONY: check
check: fmt-all vet lint test
	@echo "All checks passed!"

.PHONY: verify
verify:
	@echo "Verifying dependencies..."
	@go mod verify
	@go mod tidy

.PHONY: protoc-tools
protoc-tools: ## Download protobuf compiler and Go/gRPC codegen plugins
	@hack/install-protoc-tools.sh

.PHONY: generate
generate: ## Run go generate for protobuf code generation
	@echo "Running code generation..."
	@go generate $$(go list ./... | grep -v /hack/)
	@echo "Code generation complete"

.PHONY: clean
clean:
	@echo "Cleaning build artifacts..."
	@go clean
	@rm -f aks-flex-node-*
	@rm -f *.tar.gz
	@rm -f coverage.out coverage.html
	@rm -rf hack/bin

.PHONY: update-build-metadata
update-build-metadata:
	@echo "📅 Build Date: $(BUILD_DATE)"
	@echo "🎯 Git Commit: $(GIT_COMMIT)"
	@echo "🏷️  Version: $(VERSION)"

# Help target
.PHONY: help
help:
	@echo "AKS Flex Node Makefile"
	@echo "======================"
	@echo ""
	@echo "Build Targets:"
	@echo "  build              Build for current platform"
	@echo "  build-linux-amd64  Build for Linux AMD64"
	@echo "  build-linux-arm64  Build for Linux ARM64"
	@echo "  build-all          Build for all supported platforms"
	@echo ""
	@echo "Package Targets:"
	@echo "  package-linux-amd64 Package Linux AMD64 binary"
	@echo "  package-linux-arm64 Package Linux ARM64 binary"
	@echo "  package-all        Package all supported platforms"
	@echo ""
	@echo "Test & Quality Targets:"
	@echo "  test               Run tests"
	@echo "  test-coverage      Run tests with coverage report"
	@echo "  test-race          Run tests with race detector"
	@echo "  lint               Run golangci-lint"
	@echo "  fmt                Format code with gofmt"
	@echo "  fmt-imports        Format imports with goimports"
	@echo "  fmt-all            Format code and imports"
	@echo "  vet                Run go vet"
	@echo "  check              Run fmt-all, vet, lint, and test"
	@echo "  verify             Verify and tidy dependencies"
	@echo ""
	@echo "Other Targets:"
	@echo "  protoc-tools       Download protobuf compiler and Go/gRPC codegen plugins"
	@echo "  generate           Run go generate for protobuf code generation"
	@echo "  clean              Clean build artifacts"
	@echo "  update-build-metadata Show build metadata"
	@echo "  help               Show this help message"