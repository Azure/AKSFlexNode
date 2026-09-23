#!/usr/bin/env python3
"""Run repository-specific static checks for public Markdown documentation."""

from __future__ import annotations

import importlib.util
import re
import sys
import urllib.parse
from pathlib import Path

REPO_ROOT = Path(__file__).resolve().parents[2]
DOC_GLOBS = ("README.md", "SECURITY.md", "docs/**/*.md", "hack/*/README.md")
LINK_PATTERN = re.compile(r"(?<!!)\[[^]]*\]\(([^)]+)\)")
UNSAFE_PIPE_PATTERN = re.compile(
    r"curl\b(?:(?:\\[ \t]*\n)|[^\n|])*\|\s*(?:sudo\s+)?(?:bash|sh)\b"
)
OLD_UBUNTU_PATTERN = re.compile(r"(?:Ubuntu\s+)?22\.04|\b2204\b", re.IGNORECASE)
CONFIG_CAT_PATTERN = re.compile(r"\bcat\s+/etc/aks-flex-node/config\.json\b")


def load_url_checker():
    path = Path(__file__).with_name("check_url_contract.py")
    spec = importlib.util.spec_from_file_location("check_url_contract", path)
    if spec is None or spec.loader is None:
        raise RuntimeError(f"load {path}")
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


def markdown_files() -> list[Path]:
    paths: set[Path] = set()
    for pattern in DOC_GLOBS:
        paths.update(path for path in REPO_ROOT.glob(pattern) if path.is_file())
    return sorted(paths)


def line_number(text: str, offset: int) -> int:
    return text.count("\n", 0, offset) + 1


def check_links(files: list[Path], url_checker, errors: list[str]) -> None:
    anchor_cache: dict[Path, set[str]] = {}
    for source in files:
        text = source.read_text(encoding="utf-8")
        for match in LINK_PATTERN.finditer(text):
            raw = match.group(1).split()[0].strip("<>")
            if raw.startswith(("http:", "https:", "mailto:")):
                continue
            path_text, separator, anchor = raw.partition("#")
            target = (source.parent / urllib.parse.unquote(path_text)).resolve() if path_text else source
            location = f"{source.relative_to(REPO_ROOT)}:{line_number(text, match.start())}"
            if not target.exists():
                errors.append(f"{location}: missing local link target {raw}")
                continue
            if separator and not target.is_file():
                errors.append(f"{location}: local directory link can't contain an anchor {raw}")
                continue
            if separator:
                target_anchors = anchor_cache.setdefault(target, url_checker.anchors(target))
                if urllib.parse.unquote(anchor) not in target_anchors:
                    errors.append(f"{location}: missing local link anchor {raw}")


def check_lab_metadata(errors: list[str]) -> None:
    for path in sorted((REPO_ROOT / "docs" / "labs").glob("*.md")):
        if path.name == "README.md":
            continue
        text = path.read_text(encoding="utf-8")
        required_patterns = {
            "status": r"^> \*\*Status:\*\*",
            "validation date": r"^> \*\*(?:Last validated|Last validation attempt):\*\*",
            "host OS": r"^> \*\*Host OS:\*\*",
            "architecture": r"^> \*\*Architecture:\*\*",
        }
        for label, pattern in required_patterns.items():
            if not re.search(pattern, text, re.MULTILINE):
                errors.append(f"{path.relative_to(REPO_ROOT)}: missing lab {label} metadata")


def check_safety(files: list[Path], errors: list[str]) -> None:
    public_procedures = [
        REPO_ROOT / "README.md",
        *sorted((REPO_ROOT / "docs" / "usage").glob("*.md")),
        *sorted((REPO_ROOT / "docs" / "labs").glob("*.md")),
    ]
    for path in public_procedures:
        text = path.read_text(encoding="utf-8")
        for pattern, message in (
            (UNSAFE_PIPE_PATTERN, "downloaded script is piped directly to a shell"),
            (CONFIG_CAT_PATTERN, "credential-bearing runtime config is printed"),
        ):
            for match in pattern.finditer(text):
                errors.append(
                    f"{path.relative_to(REPO_ROOT)}:{line_number(text, match.start())}: {message}"
                )

    for path in files:
        text = path.read_text(encoding="utf-8")
        for match in OLD_UBUNTU_PATTERN.finditer(text):
            errors.append(
                f"{path.relative_to(REPO_ROOT)}:{line_number(text, match.start())}: "
                "current documentation must use Ubuntu 24.04"
            )


def main() -> int:
    files = markdown_files()
    errors: list[str] = []
    url_checker = load_url_checker()
    check_links(files, url_checker, errors)
    check_lab_metadata(errors)
    check_safety(files, errors)

    if errors:
        print("Documentation checks failed:", file=sys.stderr)
        for error in errors:
            print(f"- {error}", file=sys.stderr)
        return 1

    print(f"Documentation content checks passed ({len(files)} Markdown files).")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
