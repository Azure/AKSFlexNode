#!/usr/bin/env python3
"""Report labs whose recorded validation is older than the status policy."""

from __future__ import annotations

import argparse
import datetime as dt
import re
from pathlib import Path

REPO_ROOT = Path(__file__).resolve().parents[2]
STATUS_PATTERN = re.compile(r"^> \*\*Status:\*\*\s*(.+)$", re.MULTILINE)
DATE_PATTERN = re.compile(
    r"^> \*\*(?:Last validated|Last validation attempt):\*\*\s*(\d{4}-\d{2}-\d{2})",
    re.MULTILINE,
)


def validation_window(status: str) -> int:
    normalized = status.casefold()
    if "validated" in normalized:
        return 30
    return 90


def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument("--strict", action="store_true", help="fail when any lab is stale")
    parser.add_argument("--today", type=dt.date.fromisoformat, default=dt.date.today())
    args = parser.parse_args()

    stale: list[str] = []
    for path in sorted((REPO_ROOT / "docs" / "labs").glob("*.md")):
        if path.name == "README.md":
            continue
        text = path.read_text(encoding="utf-8")
        status_match = STATUS_PATTERN.search(text)
        date_match = DATE_PATTERN.search(text)
        relative_path = path.relative_to(REPO_ROOT)
        if status_match is None:
            stale.append(f"{relative_path}: missing status metadata")
        if date_match is None:
            stale.append(f"{relative_path}: missing validation date metadata")
        if status_match is None or date_match is None:
            continue
        status = status_match.group(1).strip()
        try:
            validated = dt.date.fromisoformat(date_match.group(1))
        except ValueError:
            stale.append(
                f"{relative_path}: invalid validation date {date_match.group(1)!r}"
            )
            continue
        window = validation_window(status)
        age = (args.today - validated).days
        if age < 0:
            stale.append(
                f"{path.relative_to(REPO_ROOT)}: validation date {validated} "
                f"is after check date {args.today}"
            )
        elif age > window:
            stale.append(
                f"{path.relative_to(REPO_ROOT)}: {age} days since validation "
                f"(policy: {window} days; status: {status})"
            )

    if not stale:
        print("Lab freshness check passed.")
        return 0

    print("Stale lab validation records:")
    for item in stale:
        print(f"- {item}")
    return 1 if args.strict else 0


if __name__ == "__main__":
    raise SystemExit(main())
