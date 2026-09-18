#!/usr/bin/env python3
"""Check stable documentation paths and anchors declared in hack/docs/url-contract.json."""

from __future__ import annotations

import html
import json
import re
import sys
from collections import defaultdict
from pathlib import Path

REPO_ROOT = Path(__file__).resolve().parents[2]
CONTRACT_PATH = Path(__file__).with_name("url-contract.json")
HEADING_PATTERN = re.compile(r"^(#{1,6})\s+(.+?)\s*#*\s*$")
EXPLICIT_ANCHOR_PATTERN = re.compile(r'<a\s+(?:name|id)=["\']([^"\']+)["\']\s*>', re.IGNORECASE)
TAG_PATTERN = re.compile(r"<[^>]+>")
MARKDOWN_LINK_PATTERN = re.compile(r"!?\[([^]]*)\]\([^)]*\)")
MARKDOWN_FORMATTING_PATTERN = re.compile(r"[`*_~]")
PUNCTUATION_PATTERN = re.compile(r"[^\w\- ]", re.UNICODE)
WHITESPACE_PATTERN = re.compile(r"\s+")


def github_slug(heading: str) -> str:
    """Return the GitHub-style base anchor for a Markdown heading."""
    value = html.unescape(heading).strip().lower()
    value = MARKDOWN_LINK_PATTERN.sub(r"\1", value)
    value = TAG_PATTERN.sub("", value)
    value = MARKDOWN_FORMATTING_PATTERN.sub("", value)
    value = PUNCTUATION_PATTERN.sub("", value)
    return WHITESPACE_PATTERN.sub("-", value).strip("-")


def anchors(path: Path) -> set[str]:
    """Collect generated heading anchors and explicit HTML anchors."""
    found: set[str] = set()
    counts: defaultdict[str, int] = defaultdict(int)
    in_fence = False

    for line in path.read_text(encoding="utf-8").splitlines():
        if line.lstrip().startswith(("```", "~~~")):
            in_fence = not in_fence
            continue
        if in_fence:
            continue

        found.update(EXPLICIT_ANCHOR_PATTERN.findall(line))
        match = HEADING_PATTERN.match(line)
        if not match:
            continue

        base = github_slug(match.group(2))
        if not base:
            continue
        occurrence = counts[base]
        counts[base] += 1
        found.add(base if occurrence == 0 else f"{base}-{occurrence}")

    return found


def inventory_paths(contract: dict[str, object]) -> set[str]:
    paths: set[str] = set()
    for pattern in contract.get("inventoryGlobs", []):
        paths.update(
            path.relative_to(REPO_ROOT).as_posix()
            for path in REPO_ROOT.glob(str(pattern))
            if path.is_file()
        )
    return paths


def main() -> int:
    contract = json.loads(CONTRACT_PATH.read_text(encoding="utf-8"))
    documents = contract.get("documents", [])
    declared: set[str] = set()
    errors: list[str] = []

    for document in documents:
        relative_path = document["path"]
        if relative_path in declared:
            errors.append(f"duplicate contract path: {relative_path}")
            continue
        declared.add(relative_path)

        path = REPO_ROOT / relative_path
        if not path.is_file():
            errors.append(f"missing protected documentation path: {relative_path}")
            continue

        if not document.get("protectAnchors", False):
            continue

        current_anchors = anchors(path)
        for anchor in document.get("anchors", []):
            if anchor not in current_anchors:
                errors.append(f"missing protected anchor: {relative_path}#{anchor}")

    missing_from_contract = inventory_paths(contract) - declared
    for relative_path in sorted(missing_from_contract):
        errors.append(f"documentation path is not recorded in URL contract: {relative_path}")

    if errors:
        print("Documentation URL contract check failed:", file=sys.stderr)
        for error in errors:
            print(f"- {error}", file=sys.stderr)
        return 1

    print(f"Documentation URL contract is valid ({len(declared)} paths).")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
