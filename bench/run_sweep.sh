#!/usr/bin/env bash
# Runs a rate sweep for one platform configuration.
#   run_sweep.sh <platform> <experiment-name> <reps> <rate> [<rate> ...]
# Repetitions are the outer loop (each repetition sweeps all rates in ascending order), with a
# cool-down between runs so that a saturated run cannot spill into the next one.
set -uo pipefail
HERE=$(cd "$(dirname "$0")" && pwd)
PLATFORM=$1 EXP=$2 REPS=$3; shift 3
RATES=("$@")
OUT=$HERE/results/$EXP
mkdir -p "$OUT"
unset HTTPS_PROXY https_proxy HTTP_PROXY http_proxy
export NO_PROXY="localhost,127.0.0.1,.example.com" no_proxy="localhost,127.0.0.1,.example.com"
if [ "$PLATFORM" = fabric ]; then
  STATE=/opt/bench/fabnet/bench_state.json
  PROCS="orderers=fabric3/bin/orderer,peers=fabric3/bin/peer node,chaincode=fabnet/bin/cc-"
  EXTRA=()
else
  STATE=/opt/bench/besunet/bench_state.json
  PROCS="validators=org.hyperledger.besu.Besu"
  EXTRA=(-artifacts "$HERE/../out")
fi
WARMUP=${WARMUP:-15s} DURATION=${DURATION:-60s} DRAIN=${DRAIN:-60s} COOLDOWN=${COOLDOWN:-45}
echo "experiment=$EXP platform=$PLATFORM reps=$REPS rates=${RATES[*]} warmup=$WARMUP duration=$DURATION" | tee "$OUT/sweep.log"
for rep in $(seq 1 "$REPS"); do
  for r in "${RATES[@]}"; do
    echo "[$(date +%T)] rep=$rep rate=$r" | tee -a "$OUT/sweep.log"
    /opt/bench/loadgen run -platform "$PLATFORM" "${EXTRA[@]}" -state "$STATE" -rate "$r" \
      -warmup "$WARMUP" -duration "$DURATION" -drain "$DRAIN" -label "r${r}_rep${rep}" -out "$OUT" \
      -procs "$PROCS" >> "$OUT/sweep.log" 2>&1
    sleep "$COOLDOWN"
  done
done
echo "[$(date +%T)] done" | tee -a "$OUT/sweep.log"
