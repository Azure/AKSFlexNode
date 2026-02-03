#!/bin/bash
# Generate SVG files from mermaid source files

set -e

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

echo "Generating diagram SVG files..."

# Check if mmdc is installed
if ! command -v mmdc &> /dev/null; then
    echo "Error: mermaid-cli (mmdc) is not installed"
    echo "Install it with: npm install -g @mermaid-js/mermaid-cli"
    exit 1
fi

# Generate each diagram in the current directory
echo "Generating system-overview.svg..."
mmdc -i "$SCRIPT_DIR/system-overview.mmd" -o "$SCRIPT_DIR/system-overview.svg" -b white

echo "Generating operational-phases.svg..."
mmdc -i "$SCRIPT_DIR/operational-phases.mmd" -o "$SCRIPT_DIR/operational-phases.svg" -b white

echo "Generating component-interactions.svg..."
mmdc -i "$SCRIPT_DIR/component-interactions.mmd" -o "$SCRIPT_DIR/component-interactions.svg" -b white

echo "Generating bootstrap-workflow.svg..."
mmdc -i "$SCRIPT_DIR/bootstrap-workflow.mmd" -o "$SCRIPT_DIR/bootstrap-workflow.svg" -b white

echo "✓ All diagrams generated successfully!"
echo "SVG files are located in: $SCRIPT_DIR"
