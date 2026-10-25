module github.com/rental-escrow/loadgen

go 1.24.7

replace (
	golang.org/x/crypto => github.com/golang/crypto v0.35.0
	golang.org/x/exp => github.com/golang/exp v0.0.0-20230626212559-97b1e661b5df
	golang.org/x/mod => github.com/golang/mod v0.22.0
	golang.org/x/net => github.com/golang/net v0.36.0
	golang.org/x/sync => github.com/golang/sync v0.11.0
	golang.org/x/sys => github.com/golang/sys v0.30.0
	golang.org/x/text => github.com/golang/text v0.22.0
	golang.org/x/time => github.com/golang/time v0.9.0
	golang.org/x/tools => github.com/golang/tools v0.29.0
	google.golang.org/genproto/googleapis/rpc => github.com/googleapis/go-genproto/googleapis/rpc v0.0.0-20250115164207-1a7da9e5054f
	google.golang.org/grpc => github.com/grpc/grpc-go v1.70.0
	google.golang.org/protobuf => github.com/protocolbuffers/protobuf-go v1.36.5
	gopkg.in/check.v1 => github.com/go-check/check v0.0.0-20161208181325-20d25e280405
	gopkg.in/yaml.v3 => github.com/go-yaml/yaml v0.0.0-20220527083530-f6f7691b1fde
	rsc.io/tmplfunc => github.com/rsc/tmplfunc v0.0.3
)

require (
	github.com/ethereum/go-ethereum v1.15.11
	github.com/hyperledger/fabric-gateway v1.7.1
	google.golang.org/grpc v1.69.2
)

require (
	github.com/bits-and-blooms/bitset v1.20.0 // indirect
	github.com/consensys/bavard v0.1.27 // indirect
	github.com/consensys/gnark-crypto v0.16.0 // indirect
	github.com/crate-crypto/go-eth-kzg v1.3.0 // indirect
	github.com/crate-crypto/go-ipa v0.0.0-20240724233137-53bbb0ceb27a // indirect
	github.com/deckarep/golang-set/v2 v2.6.0 // indirect
	github.com/decred/dcrd/dcrec/secp256k1/v4 v4.0.1 // indirect
	github.com/ethereum/c-kzg-4844/v2 v2.1.0 // indirect
	github.com/ethereum/go-verkle v0.2.2 // indirect
	github.com/gorilla/websocket v1.4.2 // indirect
	github.com/holiman/uint256 v1.3.2 // indirect
	github.com/hyperledger/fabric-protos-go-apiv2 v0.3.4 // indirect
	github.com/miekg/pkcs11 v1.1.1 // indirect
	github.com/mmcloughlin/addchain v0.4.0 // indirect
	github.com/shirou/gopsutil v3.21.4-0.20210419000835-c7a38de76ee5+incompatible // indirect
	github.com/supranational/blst v0.3.14 // indirect
	github.com/tklauser/go-sysconf v0.3.12 // indirect
	github.com/tklauser/numcpus v0.6.1 // indirect
	golang.org/x/crypto v0.35.0 // indirect
	golang.org/x/net v0.36.0 // indirect
	golang.org/x/sync v0.11.0 // indirect
	golang.org/x/sys v0.30.0 // indirect
	golang.org/x/text v0.22.0 // indirect
	google.golang.org/genproto/googleapis/rpc v0.0.0-20241202173237-19429a94021a // indirect
	google.golang.org/protobuf v1.36.3 // indirect
	rsc.io/tmplfunc v0.0.3 // indirect
)
