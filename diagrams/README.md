# Diagrams

This directory contains both the mermaid source files (`.mmd`) and generated SVG diagrams for the design documentation.

## Mermaid Source Files

- `system-overview.mmd` - Complete system architecture diagram
- `operational-phases.mmd` - Three-phase transformation process
- `component-interactions.mmd` - Bootstrap and runtime component interactions
- `bootstrap-workflow.mmd` - Sequence diagram of bootstrap workflow

## Generated SVG Files

- `system-overview.svg`
- `operational-phases.svg`
- `component-interactions.svg`
- `bootstrap-workflow.svg`

## Regenerating SVG Files

When you update a mermaid source file, regenerate the corresponding SVG:

```bash
# From the diagrams directory
./generate.sh

# Or regenerate a specific diagram
mmdc -i system-overview.mmd -o system-overview.svg -b transparent
```

## Requirements

Install mermaid-cli to generate SVG files:

```bash
npm install -g @mermaid-js/mermaid-cli
```

## Usage in Documentation

The SVG files are referenced in `docs/design.md` as:

```markdown
![System Overview](../diagrams/system-overview.svg)
![Operational Phases](../diagrams/operational-phases.svg)
![Component Interactions](../diagrams/component-interactions.svg)
![Bootstrap Workflow](../diagrams/bootstrap-workflow.svg)
```
