#!/usr/bin/env bash
# Hyperledger Besu 24.12.2 QBFT network on a single host from the official release binary:
# 4 validators. Genesis follows the Besu QBFT tutorial (block period 2 s, request timeout 4 s,
# epoch 30000) with two documented changes: Shanghai rules (required by the contract bytecode)
# and a block gas limit that does not bind (the tutorial's 4.7M gas would cap throughput).
# Usage: network.sh up [BLOCK_PERIOD_SECONDS] | down
set -euo pipefail
BESU=${BESU:-/opt/bench/besu-24.12.2/bin/besu}
NET=${NET:-/opt/bench/besunet}
HERE=$(cd "$(dirname "$0")" && pwd)
PERIOD=${2:-2}
export NO_PROXY="${NO_PROXY:-},localhost,127.0.0.1" no_proxy="${no_proxy:-},localhost,127.0.0.1"
unset HTTPS_PROXY https_proxy HTTP_PROXY http_proxy
export BESU_OPTS="-Xmx1g"
unset JAVA_TOOL_OPTIONS

log() { echo "[$(date +%T)] $*"; }

down() {
  pkill -f "org.hyperledger.besu.Besu" 2>/dev/null || true
  sleep 3
  rm -rf "$NET"
  log "network removed"
}

up() {
  down
  mkdir -p "$NET/logs"
  python3 - "$HERE/qbftConfigFile.json" "$NET/qbftConfigFile.json" "$PERIOD" <<'PY'
import json,sys
c=json.load(open(sys.argv[1])); c["genesis"]["config"]["qbft"]["blockperiodseconds"]=int(sys.argv[3])
json.dump(c,open(sys.argv[2],"w"),indent=2)
PY
  "$BESU" operator generate-blockchain-config --config-file="$NET/qbftConfigFile.json" \
    --to="$NET/networkFiles" --private-key-file-name=key > "$NET/logs/generate.log" 2>&1
  local i=0 boot=""
  for k in "$NET"/networkFiles/keys/*; do
    i=$((i+1))
    mkdir -p "$NET/node$i/data"
    cp "$k/key" "$k/key.pub" "$NET/node$i/data/"
    local args=(--data-path="$NET/node$i/data" --genesis-file="$NET/networkFiles/genesis.json"
      --p2p-port=$((30302+i)) --rpc-http-enabled --rpc-http-api=ETH,NET,QBFT,TXPOOL
      --rpc-http-port=$((8544+i)) --rpc-ws-enabled --rpc-ws-api=ETH,NET --rpc-ws-port=$((8644+i))
      --host-allowlist="*" --rpc-http-cors-origins="all" --profile=ENTERPRISE
      --metrics-enabled --metrics-port=$((9544+i)))
    if [ $i -gt 1 ]; then args+=(--bootnodes="$boot"); fi
    nohup "$BESU" "${args[@]}" > "$NET/logs/node$i.log" 2>&1 &
    if [ $i -eq 1 ]; then
      for _ in $(seq 1 60); do
        boot=$(curl -s -X POST -H 'content-type: application/json' --data '{"jsonrpc":"2.0","method":"net_enode","params":[],"id":1}' http://127.0.0.1:8545 | python3 -c 'import sys,json; print(json.load(sys.stdin)["result"])' 2>/dev/null || true)
        [ -n "$boot" ] && break; sleep 2
      done
      log "node1 enode: $boot"
    fi
  done
  for _ in $(seq 1 90); do
    local n=$(curl -s -X POST -H 'content-type: application/json' --data '{"jsonrpc":"2.0","method":"eth_blockNumber","params":[],"id":1}' http://127.0.0.1:8545 | python3 -c 'import sys,json; print(int(json.load(sys.stdin)["result"],16))' 2>/dev/null || echo 0)
    if [ "$n" -ge 3 ]; then log "QBFT producing blocks (height $n, block period ${PERIOD}s)"; return 0; fi
    sleep 2
  done
  log "network did not start"; exit 1
}

case "${1:-}" in
  up) up ;;
  down) down ;;
  *) echo "usage: $0 up [block_period_s] | down"; exit 1 ;;
esac
