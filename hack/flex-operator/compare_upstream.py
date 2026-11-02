#!/usr/bin/env python3
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.
"""Compare the flex-operator chart render with upstream unbounded-operator manifests.

Usage: compare_upstream.py <chart-render.yaml|-> <upstream-rendered-dir>
Prints differences per object, ignoring labels/annotations that the chart
intentionally adds or computes differently. Exits non-zero on unexpected drift.
"""
import glob, sys, yaml

IGNORED = {
    ("metadata", "labels"),
    ("spec", "template", "metadata", "annotations", "unbounded-cloud.io/operator-config-hash"),
}
EXPECTED = {
    ("Deployment", "unbounded-operator", "spec.template.spec.containers[0].imagePullPolicy"),
    ("Deployment", "unbounded-operator", "spec.strategy.rollingUpdate"),
}

def load(paths):
    out = {}
    for p in paths:
        for d in yaml.safe_load_all(sys.stdin if p == "-" else open(p)):
            if d:
                out[(d["kind"], d["metadata"]["name"])] = d
    return out

def strip(d):
    d = yaml.safe_load(yaml.safe_dump(d))
    d["metadata"].pop("labels", None)
    ann = d.get("spec", {}).get("template", {}).get("metadata", {}).get("annotations", {})
    ann.pop("unbounded-cloud.io/operator-config-hash", None)
    return d

def diff(a, b, path=""):
    if isinstance(a, dict) and isinstance(b, dict):
        for k in sorted(set(a) | set(b)):
            yield from diff(a.get(k), b.get(k), f"{path}.{k}" if path else k)
    elif isinstance(a, list) and isinstance(b, list) and len(a) == len(b):
        for i, (x, y) in enumerate(zip(a, b)):
            yield from diff(x, y, f"{path}[{i}]")
    elif a != b:
        yield path, a, b

chart = load([sys.argv[1]])
upstream = load(sorted(glob.glob(sys.argv[2] + "/*.yaml")))
bad = 0
for key in sorted(upstream):
    if key[0] == "Namespace":
        print(f"{key}: created as release namespace (expected)")
        continue
    if key not in chart:
        print(f"{key}: MISSING from chart"); bad += 1; continue
    for path, up, ch in diff(strip(upstream[key]), strip(chart[key])):
        tag = "expected" if (key[0], key[1], path) in EXPECTED else "UNEXPECTED"
        bad += tag == "UNEXPECTED"
        print(f"{key} {path}: upstream={up!r} chart={ch!r} ({tag})")
extra = sorted(set(chart) - set(upstream))
print("chart-only objects:", extra)
print("RESULT:", "OK" if bad == 0 else f"{bad} unexpected difference(s)")
sys.exit(1 if bad else 0)
