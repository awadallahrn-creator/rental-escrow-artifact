#!/usr/bin/env bash
# Full measurement programme after the main Besu sweep (run_sweep.sh besu besu_main ...).
set -uo pipefail
HERE=$(cd "$(dirname "$0")" && pwd)
cd "$HERE"
unset HTTPS_PROXY https_proxy HTTP_PROXY http_proxy
export NO_PROXY="localhost,127.0.0.1,.example.com" no_proxy="localhost,127.0.0.1,.example.com"
export GOPROXY=direct GOFLAGS=-mod=mod GOSUMDB=off GOTOOLCHAIN=local
log() { echo "[$(date +%T)] $*" | tee -a "$HERE/results/pipeline.log"; }
RATES_MAIN=(10 25 50 75 100 125 150)
RATES_SENS=(25 50 75 100)

until grep -q "done" results/besu_main/sweep.log 2>/dev/null; do sleep 20; done
log "besu_main finished"

# 1. Besu (2 s block period) functional check with the final client
/opt/bench/loadgen e2e -platform besu -artifacts ../out -out results/e2e >> results/pipeline.log 2>&1 && log "besu e2e ok" || log "besu e2e FAILED"

# 2. Besu sensitivity: 1 s block period
besu/network.sh up 1 >> results/pipeline.log 2>&1
/opt/bench/loadgen setup -platform besu -leases 1000 -artifacts ../out -state /opt/bench/besunet/bench_state.json >> results/pipeline.log 2>&1
./run_sweep.sh besu besu_period1s 3 "${RATES_SENS[@]}"
besu/network.sh down >> results/pipeline.log 2>&1
log "besu_period1s finished"

# 3. Fabric main (unanimous endorsement, as in the original manuscript)
fabric/network.sh up >> results/pipeline.log 2>&1
/opt/bench/loadgen e2e -platform fabric -out results/e2e >> results/pipeline.log 2>&1 && log "fabric e2e ok" || log "fabric e2e FAILED"
/opt/bench/loadgen setup -platform fabric -leases 1000 -state /opt/bench/fabnet/bench_state.json >> results/pipeline.log 2>&1
./run_sweep.sh fabric fabric_main 3 "${RATES_MAIN[@]}"
log "fabric_main finished"

# 4. Fabric sensitivity: two-of-three endorsement
fabric/network.sh up "OutOf(2,'Org1MSP.peer','Org2MSP.peer','Org3MSP.peer')" >> results/pipeline.log 2>&1
/opt/bench/loadgen setup -platform fabric -leases 1000 -state /opt/bench/fabnet/bench_state.json >> results/pipeline.log 2>&1
./run_sweep.sh fabric fabric_majority 3 "${RATES_SENS[@]}"
fabric/network.sh down >> results/pipeline.log 2>&1
log "pipeline done"
