#!/usr/bin/env bash
# Hyperledger Fabric 3.1.1 network on a single host, run from official release binaries:
#   4 SmartBFT orderers (OrdererOrg) + 3 peers (Org1, Org2, Org3), TLS everywhere,
#   one channel, chaincode-as-a-service for `token` and `rental`.
# Node settings mirror fabric-samples test-network (compose-bft-test-net.yaml); only ports
# and filesystem paths differ because all nodes share one host without containers.
#
# Usage: network.sh up [ENDORSEMENT_POLICY] | down
set -euo pipefail

FABRIC=${FABRIC:-/opt/bench/fabric3}
NET=${NET:-/opt/bench/fabnet}
HERE=$(cd "$(dirname "$0")" && pwd)
CC_SRC=${CC_SRC:-$HERE/../../fabric/chaincode}
CHANNEL=rentalchannel
export PATH=$FABRIC/bin:$PATH
# Local node-to-node and CLI traffic must never go through an outbound HTTP proxy.
export NO_PROXY="${NO_PROXY:-},.example.com,localhost,127.0.0.1" no_proxy="${no_proxy:-},.example.com,localhost,127.0.0.1"
unset HTTPS_PROXY https_proxy HTTP_PROXY http_proxy grpc_proxy
CRYPTO=$NET/crypto
ORDS=(orderer orderer2 orderer3 orderer4)
ORD_PORT=(7050 7052 7056 7058)      # reference ports
ORD_ADMIN=(7053 7055 7057 7059)     # reference admin ports
ORD_OPS=(9443 9446 9447 9448)
PEER_PORT=(7151 8151 9151)
PEER_CC=(7152 8152 9152)
PEER_OPS=(9451 9452 9453)
POLICY=${2:-"AND('Org1MSP.peer','Org2MSP.peer','Org3MSP.peer')"}

log() { echo "[$(date +%T)] $*"; }

down() {
  pkill -f "$FABRIC/bin/orderer" 2>/dev/null || true
  pkill -f "$FABRIC/bin/peer node start" 2>/dev/null || true
  pkill -f "$NET/bin/cc-" 2>/dev/null || true
  sleep 2
  rm -rf "$NET"
  log "network removed"
}

start_orderer() {
  local i=$1 h=${ORDS[$1]} d=$CRYPTO/ordererOrganizations/example.com/orderers/${ORDS[$1]}.example.com
  mkdir -p "$NET/data/$h" "$NET/logs"
  env FABRIC_CFG_PATH=$FABRIC/config FABRIC_LOGGING_SPEC=INFO \
    ORDERER_GENERAL_LISTENADDRESS=0.0.0.0 ORDERER_GENERAL_LISTENPORT=${ORD_PORT[$i]} \
    ORDERER_GENERAL_LOCALMSPID=OrdererMSP ORDERER_GENERAL_LOCALMSPDIR=$d/msp \
    ORDERER_GENERAL_TLS_ENABLED=true ORDERER_GENERAL_TLS_PRIVATEKEY=$d/tls/server.key \
    ORDERER_GENERAL_TLS_CERTIFICATE=$d/tls/server.crt ORDERER_GENERAL_TLS_ROOTCAS="[$d/tls/ca.crt]" \
    ORDERER_GENERAL_CLUSTER_CLIENTCERTIFICATE=$d/tls/server.crt ORDERER_GENERAL_CLUSTER_CLIENTPRIVATEKEY=$d/tls/server.key \
    ORDERER_GENERAL_CLUSTER_ROOTCAS="[$d/tls/ca.crt]" ORDERER_GENERAL_BOOTSTRAPMETHOD=none \
    ORDERER_CHANNELPARTICIPATION_ENABLED=true ORDERER_ADMIN_TLS_ENABLED=true \
    ORDERER_ADMIN_TLS_CERTIFICATE=$d/tls/server.crt ORDERER_ADMIN_TLS_PRIVATEKEY=$d/tls/server.key \
    ORDERER_ADMIN_TLS_ROOTCAS="[$d/tls/ca.crt]" ORDERER_ADMIN_TLS_CLIENTROOTCAS="[$d/tls/ca.crt]" \
    ORDERER_ADMIN_LISTENADDRESS=0.0.0.0:${ORD_ADMIN[$i]} \
    ORDERER_OPERATIONS_LISTENADDRESS=127.0.0.1:${ORD_OPS[$i]} ORDERER_METRICS_PROVIDER=prometheus \
    ORDERER_FILELEDGER_LOCATION=$NET/data/$h/ledger \
    ORDERER_CONSENSUS_WALDIR=$NET/data/$h/wal ORDERER_CONSENSUS_SNAPDIR=$NET/data/$h/snap \
    nohup $FABRIC/bin/orderer > "$NET/logs/$h.log" 2>&1 &
}

start_peer() {
  local i=$1 n=$(( $1 + 1 )) d=$CRYPTO/peerOrganizations/org$(( $1 + 1 )).example.com/peers/peer0.org$(( $1 + 1 )).example.com
  local host=peer0.org$n.example.com
  mkdir -p "$NET/data/$host" "$NET/logs"
  env FABRIC_CFG_PATH=$HERE/peercfg FABRIC_LOGGING_SPEC=INFO \
    CORE_PEER_TLS_ENABLED=true CORE_PEER_PROFILE_ENABLED=false \
    CORE_PEER_TLS_CERT_FILE=$d/tls/server.crt CORE_PEER_TLS_KEY_FILE=$d/tls/server.key \
    CORE_PEER_TLS_ROOTCERT_FILE=$d/tls/ca.crt \
    CORE_PEER_ID=$host CORE_PEER_ADDRESS=$host:${PEER_PORT[$i]} \
    CORE_PEER_LISTENADDRESS=0.0.0.0:${PEER_PORT[$i]} \
    CORE_PEER_CHAINCODEADDRESS=$host:${PEER_CC[$i]} CORE_PEER_CHAINCODELISTENADDRESS=0.0.0.0:${PEER_CC[$i]} \
    CORE_PEER_GOSSIP_BOOTSTRAP=$host:${PEER_PORT[$i]} CORE_PEER_GOSSIP_EXTERNALENDPOINT=$host:${PEER_PORT[$i]} \
    CORE_PEER_LOCALMSPID=Org${n}MSP CORE_PEER_MSPCONFIGPATH=$d/msp \
    CORE_OPERATIONS_LISTENADDRESS=127.0.0.1:${PEER_OPS[$i]} CORE_METRICS_PROVIDER=prometheus \
    CHAINCODE_AS_A_SERVICE_BUILDER_CONFIG="{\"peername\":\"peer0org$n\"}" CORE_CHAINCODE_EXECUTETIMEOUT=300s \
    CORE_PEER_FILESYSTEMPATH=$NET/data/$host CORE_VM_ENDPOINT="" \
    nohup $FABRIC/bin/peer node start > "$NET/logs/$host.log" 2>&1 &
}

as_admin() { # org number -> sets CLI env for that org's admin
  local n=$1 i=$(( $1 - 1 ))
  export FABRIC_CFG_PATH=$HERE/peercfg CORE_PEER_TLS_ENABLED=true CORE_PEER_LOCALMSPID=Org${n}MSP
  export CORE_PEER_MSPCONFIGPATH=$CRYPTO/peerOrganizations/org$n.example.com/users/Admin@org$n.example.com/msp
  export CORE_PEER_TLS_ROOTCERT_FILE=$CRYPTO/peerOrganizations/org$n.example.com/peers/peer0.org$n.example.com/tls/ca.crt
  export CORE_PEER_ADDRESS=peer0.org$n.example.com:${PEER_PORT[$i]}
}

ORDERER_CA=() # filled in up()

deploy_cc() { # name port
  local name=$1 port=$2 pkg=$NET/pkg/$1
  mkdir -p "$pkg/src"
  printf '{"address":"127.0.0.1:%s","dial_timeout":"10s","tls_required":false}' "$port" > "$pkg/src/connection.json"
  printf '{"type":"ccaas","label":"%s_1.0"}' "$name" > "$pkg/metadata.json"
  tar -C "$pkg/src" -czf "$pkg/code.tar.gz" connection.json
  tar -C "$pkg" -czf "$NET/pkg/$name.tgz" metadata.json code.tar.gz
  local id=""
  for n in 1 2 3; do
    as_admin $n
    peer lifecycle chaincode install "$NET/pkg/$name.tgz" >> "$NET/logs/cli.log" 2>&1
  done
  id=$(peer lifecycle chaincode calculatepackageid "$NET/pkg/$name.tgz")
  # chaincode server process (one process serves all three peers)
  ( cd "$CC_SRC" && go build -o "$NET/bin/cc-$name" "./cmd/$name" )
  env CHAINCODE_ID="$id" CHAINCODE_SERVER_ADDRESS=127.0.0.1:$port CHAINCODE_NAME=$name \
    nohup "$NET/bin/cc-$name" > "$NET/logs/cc-$name.log" 2>&1 &
  sleep 1
  for n in 1 2 3; do
    as_admin $n
    peer lifecycle chaincode approveformyorg -o orderer.example.com:7050 --tls --cafile "$ORDERER_CA" \
      --channelID $CHANNEL --name "$name" --version 1.0 --package-id "$id" --sequence 1 \
      --signature-policy "$POLICY" >> "$NET/logs/cli.log" 2>&1
  done
  as_admin 1
  peer lifecycle chaincode commit -o orderer.example.com:7050 --tls --cafile "$ORDERER_CA" \
    --channelID $CHANNEL --name "$name" --version 1.0 --sequence 1 --signature-policy "$POLICY" \
    --peerAddresses peer0.org1.example.com:7151 --tlsRootCertFiles $CRYPTO/peerOrganizations/org1.example.com/peers/peer0.org1.example.com/tls/ca.crt \
    --peerAddresses peer0.org2.example.com:8151 --tlsRootCertFiles $CRYPTO/peerOrganizations/org2.example.com/peers/peer0.org2.example.com/tls/ca.crt \
    --peerAddresses peer0.org3.example.com:9151 --tlsRootCertFiles $CRYPTO/peerOrganizations/org3.example.com/peers/peer0.org3.example.com/tls/ca.crt \
    >> "$NET/logs/cli.log" 2>&1
  log "chaincode $name committed (package $id, policy $POLICY)"
}

up() {
  down
  mkdir -p "$NET/logs" "$NET/pkg" "$NET/bin"
  cryptogen generate --config="$HERE/crypto-config.yaml" --output="$CRYPTO" > /dev/null
  configtxgen -configPath "$HERE" -profile ChannelUsingBFT -outputBlock "$NET/$CHANNEL.block" -channelID $CHANNEL \
    > "$NET/logs/configtxgen.log" 2>&1
  log "crypto material and genesis block generated"

  for i in 0 1 2 3; do start_orderer $i; done
  sleep 4
  for i in 0 1 2 3; do
    local d=$CRYPTO/ordererOrganizations/example.com/orderers/${ORDS[$i]}.example.com
    osnadmin channel join --channelID $CHANNEL --config-block "$NET/$CHANNEL.block" \
      -o localhost:${ORD_ADMIN[$i]} --ca-file $d/tls/ca.crt --client-cert $d/tls/server.crt \
      --client-key $d/tls/server.key >> "$NET/logs/cli.log" 2>&1
  done
  log "4 BFT orderers joined channel"

  for i in 0 1 2; do start_peer $i; done
  sleep 5
  ORDERER_CA=$CRYPTO/ordererOrganizations/example.com/orderers/orderer.example.com/tls/ca.crt
  for n in 1 2 3; do
    as_admin $n
    peer channel join -b "$NET/$CHANNEL.block" >> "$NET/logs/cli.log" 2>&1
  done
  log "3 peers joined channel"
  sleep 3

  deploy_cc token 9991
  deploy_cc rental 9992
  sleep 30 # let gossip membership converge so service discovery sees all organisations
  log "network ready"
}

case "${1:-}" in
  up) up ;;
  down) down ;;
  *) echo "usage: $0 up [policy] | down"; exit 1 ;;
esac
