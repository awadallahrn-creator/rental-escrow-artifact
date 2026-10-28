#!/usr/bin/env python3
"""Compare gas receipts: reference (Cancun, reported) vs Shanghai run vs a second Cancun run."""
import csv
import sys

ref, sh, c2 = (list(csv.DictReader(open(p))) for p in sys.argv[1:4])
assert len(ref) == len(sh) == len(c2)
key = lambda r: (r["scenario"], r["step"], r["function"])  # noqa: E731
assert [key(r) for r in ref] == [key(r) for r in sh] == [key(r) for r in c2]


def diff(a, b):
    return [(x["scenario"], x["step"], int(x["gas_used"]), int(y["gas_used"])) for x, y in zip(a, b)
            if x["gas_used"] != y["gas_used"]]


d_sh, d_c2 = diff(ref, sh), diff(ref, c2)
print(f"receipts: {len(ref)}")
print(f"Shanghai vs reported Cancun: {len(ref) - len(d_sh)} identical, {len(d_sh)} different")
for s, step, a, b in d_sh:
    print(f"  {s} | {step}: {a} -> {b} ({b - a:+d})")
print(f"second Cancun run vs reported Cancun: {len(ref) - len(d_c2)} identical, {len(d_c2)} different")
for s, step, a, b in d_c2:
    print(f"  {s} | {step}: {a} -> {b} ({b - a:+d})")
fns = {x["function"] for x in ref for y in [x] if (x["scenario"], x["step"]) in {(s, st) for s, st, _, _ in d_sh + d_c2}}
print("functions with differences:", ", ".join(sorted(fns)) or "none")
