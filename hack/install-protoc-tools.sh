#!/usr/bin/env bash
# Download and install the protobuf compiler and Go/gRPC codegen plugins.
# Binaries are installed into hack/bin relative to the repository root.

set -euo pipefail

# Versions
PROTOC_VERSION="${PROTOC_VERSION:-33.5}"
PROTOC_GEN_GO_VERSION="${PROTOC_GEN_GO_VERSION:-v1.36.11}"
PROTOC_GEN_GO_GRPC_VERSION="${PROTOC_GEN_GO_GRPC_VERSION:-v1.6.1}"

# Resolve repository root and install directory
REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
INSTALL_DIR="${REPO_ROOT}/hack/bin"

# Detect OS
case "$(uname -s)" in
  Linux)  PROTOC_OS="linux" ;;
  Darwin) PROTOC_OS="osx" ;;
  *)      echo "Unsupported OS: $(uname -s)" >&2; exit 1 ;;
esac

# Detect architecture
case "$(uname -m)" in
  x86_64)       PROTOC_ARCH="x86_64" ;;
  aarch64|arm64) PROTOC_ARCH="aarch_64" ;;
  *)             echo "Unsupported architecture: $(uname -m)" >&2; exit 1 ;;
esac

PROTOC_ZIP="protoc-${PROTOC_VERSION}-${PROTOC_OS}-${PROTOC_ARCH}.zip"
PROTOC_URL="https://github.com/protocolbuffers/protobuf/releases/download/v${PROTOC_VERSION}/${PROTOC_ZIP}"

mkdir -p "${INSTALL_DIR}"

echo "Installing protoc ${PROTOC_VERSION}..."
curl -sSfL "${PROTOC_URL}" -o "${INSTALL_DIR}/${PROTOC_ZIP}"
unzip -qo "${INSTALL_DIR}/${PROTOC_ZIP}" -d "${INSTALL_DIR}"
rm -f "${INSTALL_DIR}/${PROTOC_ZIP}"
echo "protoc installed to ${INSTALL_DIR}/bin/protoc"

echo "Installing protoc-gen-go ${PROTOC_GEN_GO_VERSION}..."
GOBIN="${INSTALL_DIR}" go install "google.golang.org/protobuf/cmd/protoc-gen-go@${PROTOC_GEN_GO_VERSION}"

echo "Installing protoc-gen-go-grpc ${PROTOC_GEN_GO_GRPC_VERSION}..."
GOBIN="${INSTALL_DIR}" go install "google.golang.org/grpc/cmd/protoc-gen-go-grpc@${PROTOC_GEN_GO_GRPC_VERSION}"

echo ""
echo "Protobuf toolchain installed to ${INSTALL_DIR}"
echo "To use, add it to your PATH:"
echo "  export PATH=\"${INSTALL_DIR}/bin:${INSTALL_DIR}:\${PATH}\""
