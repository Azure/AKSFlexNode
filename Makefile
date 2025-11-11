# AKS FlexNode Makefile
BUILD_DATE := $(shell date -u +"%Y-%m-%dT%H:%M:%SZ")
GIT_COMMIT := $(shell git rev-parse --short HEAD 2>/dev/null || echo "unknown")

.PHONY: build
build:
	@go build .

.PHONY: test
test:
	@go test ./...

.PHONY: clean
clean:
	@go clean

.PHONY: update-build-metadata
update-build-metadata:
	@echo "📅 Build Date: $(BUILD_DATE)"
	@echo "🎯 Git Commit: $(GIT_COMMIT)"