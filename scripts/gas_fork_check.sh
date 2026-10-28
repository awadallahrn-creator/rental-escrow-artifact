#!/usr/bin/env bash
# Fork check for the gas figures (manuscript Section 5.2): the contracts are compiled for Shanghai,
# gas is reported under Cancun rules. This script measures the same five leases under Shanghai rules
# and again under Cancun rules (a second run, to show run-to-run variation), then compares both with
# results/gas_receipts.csv. Run from the project root after `forge build`.
set -euo pipefail
OUT=${1:-results/gas_fork_check/rerun}
python3 scripts/measure_gas.py --hardfork shanghai --out "$OUT/shanghai" > /dev/null
python3 scripts/measure_gas.py --hardfork cancun   --out "$OUT/cancun_rerun" > /dev/null
python3 scripts/compare_gas.py results/gas_receipts.csv "$OUT/shanghai/gas_receipts.csv" "$OUT/cancun_rerun/gas_receipts.csv" | tee "$OUT/comparison.txt"
