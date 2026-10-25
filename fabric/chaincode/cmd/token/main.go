// Chaincode-as-a-service entry point for the token chaincode.
package main

import (
	"log"
	"os"

	"github.com/hyperledger/fabric-chaincode-go/v2/shim"
	"github.com/hyperledger/fabric-contract-api-go/v2/contractapi"
	"github.com/rental-escrow/chaincode/token"
)

func main() {
	cc, err := contractapi.NewChaincode(&token.Contract{})
	if err != nil {
		log.Fatal(err)
	}
	server := &shim.ChaincodeServer{
		CCID:     os.Getenv("CHAINCODE_ID"),
		Address:  os.Getenv("CHAINCODE_SERVER_ADDRESS"),
		CC:       cc,
		TLSProps: shim.TLSProperties{Disabled: true},
	}
	log.Fatal(server.Start())
}
