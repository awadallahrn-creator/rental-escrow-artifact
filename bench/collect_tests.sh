#!/usr/bin/env bash
# Runs every test suite and the static analyser, and records the counts used in the manuscript
# (bench/results/tests.json). Run after the benchmarks, never during them (it loads the CPU).
set -euo pipefail
ROOT=$(cd "$(dirname "$0")/.." && pwd)
OUT=$ROOT/bench/results/tests.json
export FOUNDRY_SOLC=/opt/foundry/solc PATH=/opt/foundry:$PATH GOPROXY=direct GOFLAGS=-mod=mod GOSUMDB=off GOTOOLCHAIN=local

cd "$ROOT"
forge test --use /opt/foundry/solc > /tmp/forge_test.log 2>&1
grep -q "0 failed" /tmp/forge_test.log
ETH_ALL=$(grep -oE "[0-9]+ tests passed" /tmp/forge_test.log | tail -1 | grep -oE "^[0-9]+")
ETH_FUZZ=$(grep -c "testFuzz_" /tmp/forge_test.log || true)
ETH_INV=$(grep -c "invariant_" /tmp/forge_test.log || true)
ETH_UNIT=$((ETH_ALL - ETH_FUZZ - ETH_INV))
FUZZ_RUNS=$(grep -oE "testFuzz_[A-Za-z]+\([^)]*\) \(runs: [0-9]+" /tmp/forge_test.log | grep -oE "[0-9]+$" | head -1)
INV_RUNS=$(grep -oE "^runs = [0-9]+" foundry.toml | tail -1 | grep -oE "[0-9]+")
INV_DEPTH=$(grep -oE "^depth = [0-9]+" foundry.toml | grep -oE "[0-9]+")

cd "$ROOT/fabric/chaincode"
go test ./rental/ -v -count=1 > /tmp/go_test.log 2>&1
grep -q "^ok" /tmp/go_test.log
FAB_ALL=$(grep -c -- "--- PASS" /tmp/go_test.log)
FAB_UNIT=$((FAB_ALL - 2))   # minus the property test and the invariant suite
FAB_PROP=$(grep -oE "for run := 0; run < [0-9]+" rental/rental_test.go | head -1 | grep -oE "[0-9]+$")
FAB_INV=$(grep -oE "for run := 0; run < [0-9]+" rental/rental_test.go | tail -1 | grep -oE "[0-9]+$")
FAB_DEPTH=$(grep -oE "for step := 0; step < [0-9]+" rental/rental_test.go | grep -oE "[0-9]+$")

cd "$ROOT"
slither . --filter-paths "lib/|test/" > "$ROOT/results/slither_report.txt" 2>&1 || true
SLITHER=$(grep -oE "[0-9]+ result\(s\) found" "$ROOT/results/slither_report.txt" | grep -oE "^[0-9]+")

cat > "$OUT" <<EOF
{
  "EthUnitTests": $ETH_UNIT,
  "EthFuzzRuns": "$(printf "%'d" "$FUZZ_RUNS" | sed 's/,/{,}/g')",
  "EthInvariantRuns": $INV_RUNS,
  "EthInvariantDepth": $INV_DEPTH,
  "FabUnitTests": $FAB_UNIT,
  "FabPropertyRuns": $FAB_PROP,
  "FabInvariantRuns": $FAB_INV,
  "FabInvariantDepth": $FAB_DEPTH,
  "SlitherFindings": $SLITHER
}
EOF
cat "$OUT"
