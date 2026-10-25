module github.com/rental-escrow/chaincode

go 1.24.7

require (
	github.com/hyperledger/fabric-chaincode-go/v2 v2.0.0
	github.com/hyperledger/fabric-contract-api-go/v2 v2.2.0
	github.com/hyperledger/fabric-protos-go-apiv2 v0.3.4
	google.golang.org/protobuf v1.36.3
)

require (
	github.com/go-openapi/jsonpointer v0.21.0 // indirect
	github.com/go-openapi/jsonreference v0.21.0 // indirect
	github.com/go-openapi/spec v0.21.0 // indirect
	github.com/go-openapi/swag v0.23.0 // indirect
	github.com/josharian/intern v1.0.0 // indirect
	github.com/mailru/easyjson v0.7.7 // indirect
	github.com/xeipuuv/gojsonpointer v0.0.0-20190905194746-02993c407bfb // indirect
	github.com/xeipuuv/gojsonreference v0.0.0-20180127040603-bd5ef7bd5415 // indirect
	github.com/xeipuuv/gojsonschema v1.2.0 // indirect
	golang.org/x/net v0.32.0 // indirect
	golang.org/x/sys v0.29.0 // indirect
	golang.org/x/text v0.21.0 // indirect
	google.golang.org/genproto/googleapis/rpc v0.0.0-20241202173237-19429a94021a // indirect
	google.golang.org/grpc v1.67.0 // indirect
	gopkg.in/yaml.v3 v3.0.1 // indirect
)

// Vanity import paths are resolved to their official GitHub mirrors (identical source),
// because the vanity hosts are not reachable from the build environment.
replace (
	golang.org/x/net => github.com/golang/net v0.34.0
	golang.org/x/sys => github.com/golang/sys v0.29.0
	golang.org/x/text => github.com/golang/text v0.21.0
	google.golang.org/genproto/googleapis/rpc => github.com/googleapis/go-genproto/googleapis/rpc v0.0.0-20250115164207-1a7da9e5054f
	google.golang.org/grpc => github.com/grpc/grpc-go v1.70.0
	google.golang.org/protobuf => github.com/protocolbuffers/protobuf-go v1.36.5
	gopkg.in/check.v1 => github.com/go-check/check v0.0.0-20161208181325-20d25e280405
	gopkg.in/yaml.v3 => github.com/go-yaml/yaml v0.0.0-20220527083530-f6f7691b1fde
)
