// Package token implements a minimal fungible-token chaincode used as the rent and deposit
// currency on Hyperledger Fabric. It plays the role that the ERC-20 stablecoin plays on
// Ethereum: the rental chaincode never stores balances itself; it moves funds by invoking
// this chaincode on the same channel.
//
// Accounts are Fabric client identity IDs (cid.GetID). Escrow accounts are reserved identifiers
// "escrow:<operator chaincode>:<sub-account>", one per lease, so that deposits of different leases
// never touch the same ledger key (no MVCC hotspot; this mirrors one balance per lease contract
// on Ethereum). Only transactions whose top-level chaincode is the configured operator (the
// rental chaincode) may release funds from an escrow account.
package token

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"

	"github.com/hyperledger/fabric-chaincode-go/v2/pkg/cid"
	"github.com/hyperledger/fabric-contract-api-go/v2/contractapi"
	"github.com/hyperledger/fabric-protos-go-apiv2/peer"
	"google.golang.org/protobuf/proto"
)

const (
	keyMinter   = "meta:minter"
	keyOperator = "meta:operator"
	prefixBal   = "bal:"
)

// EscrowAccount returns the reserved escrow account identifier of an operator sub-account.
// Client identity IDs are base64 strings and can never contain ':', so no collision is possible.
func EscrowAccount(operator, sub string) string { return "escrow:" + operator + ":" + sub }

// Contract is the token chaincode.
type Contract struct {
	contractapi.Contract
}

// TransferEvent is emitted on every balance movement.
type TransferEvent struct {
	From   string `json:"from"`
	To     string `json:"to"`
	Amount int64  `json:"amount"`
}

// Initialize sets the caller as minter and names the chaincode allowed to release escrow.
func (c *Contract) Initialize(ctx contractapi.TransactionContextInterface, operatorChaincode string) error {
	stub := ctx.GetStub()
	existing, err := stub.GetState(keyMinter)
	if err != nil {
		return err
	}
	if existing != nil {
		return errors.New("already initialized")
	}
	if operatorChaincode == "" {
		return errors.New("operator chaincode required")
	}
	minter, err := cid.GetID(stub)
	if err != nil {
		return err
	}
	if err := stub.PutState(keyMinter, []byte(minter)); err != nil {
		return err
	}
	return stub.PutState(keyOperator, []byte(operatorChaincode))
}

// Mint creates new tokens (test currency issuance; restricted to the minter).
func (c *Contract) Mint(ctx contractapi.TransactionContextInterface, account string, amount int64) error {
	stub := ctx.GetStub()
	caller, err := cid.GetID(stub)
	if err != nil {
		return err
	}
	minter, err := stub.GetState(keyMinter)
	if err != nil {
		return err
	}
	if string(minter) != caller {
		return errors.New("unauthorized: only minter")
	}
	if amount <= 0 {
		return errors.New("amount must be positive")
	}
	return credit(ctx, account, amount)
}

// BalanceOf returns an account balance.
func (c *Contract) BalanceOf(ctx contractapi.TransactionContextInterface, account string) (int64, error) {
	return balance(ctx, account)
}

// ClientAccountID returns the caller's account identifier.
func (c *Contract) ClientAccountID(ctx contractapi.TransactionContextInterface) (string, error) {
	return cid.GetID(ctx.GetStub())
}

// Transfer moves tokens from the transaction creator to `to`. When invoked by another
// chaincode, the creator is still the end user who signed the proposal, so the user's
// signature is the authorisation (no separate approve step is needed on Fabric).
func (c *Contract) Transfer(ctx contractapi.TransactionContextInterface, to string, amount int64) error {
	from, err := cid.GetID(ctx.GetStub())
	if err != nil {
		return err
	}
	return move(ctx, from, to, amount)
}

// ReleaseEscrow moves tokens out of an escrow account of the operator. Allowed only when the
// top-level chaincode of the transaction is the configured operator (the rental chaincode),
// which enforces its own authorisation rules before invoking this function.
func (c *Contract) ReleaseEscrow(ctx contractapi.TransactionContextInterface, escrow string, to string, amount int64) error {
	stub := ctx.GetStub()
	operator, err := stub.GetState(keyOperator)
	if err != nil {
		return err
	}
	if operator == nil {
		return errors.New("not initialized")
	}
	top, err := TopLevelChaincode(ctx)
	if err != nil {
		return err
	}
	if top != string(operator) {
		return fmt.Errorf("unauthorized: escrow release must originate from %s", operator)
	}
	if !strings.HasPrefix(escrow, EscrowAccount(string(operator), "")) {
		return errors.New("unauthorized: not an escrow account of the operator")
	}
	return move(ctx, escrow, to, amount)
}

// TopLevelChaincode returns the name of the chaincode the client proposal targeted.
func TopLevelChaincode(ctx contractapi.TransactionContextInterface) (string, error) {
	sp, err := ctx.GetStub().GetSignedProposal()
	if err != nil {
		return "", err
	}
	if sp == nil {
		return "", errors.New("no signed proposal")
	}
	prop := &peer.Proposal{}
	if err := proto.Unmarshal(sp.ProposalBytes, prop); err != nil {
		return "", err
	}
	payload := &peer.ChaincodeProposalPayload{}
	if err := proto.Unmarshal(prop.Payload, payload); err != nil {
		return "", err
	}
	spec := &peer.ChaincodeInvocationSpec{}
	if err := proto.Unmarshal(payload.Input, spec); err != nil {
		return "", err
	}
	if spec.ChaincodeSpec == nil || spec.ChaincodeSpec.ChaincodeId == nil {
		return "", errors.New("malformed proposal")
	}
	return spec.ChaincodeSpec.ChaincodeId.Name, nil
}

func move(ctx contractapi.TransactionContextInterface, from, to string, amount int64) error {
	if amount <= 0 {
		return errors.New("amount must be positive")
	}
	if to == "" || to == from {
		return errors.New("invalid recipient")
	}
	fromBal, err := balance(ctx, from)
	if err != nil {
		return err
	}
	if fromBal < amount {
		return errors.New("insufficient balance")
	}
	if err := putBalance(ctx, from, fromBal-amount); err != nil {
		return err
	}
	if err := credit(ctx, to, amount); err != nil {
		return err
	}
	ev, _ := json.Marshal(TransferEvent{From: from, To: to, Amount: amount})
	return ctx.GetStub().SetEvent("Transfer", ev)
}

func credit(ctx contractapi.TransactionContextInterface, account string, amount int64) error {
	bal, err := balance(ctx, account)
	if err != nil {
		return err
	}
	if bal > math.MaxInt64-amount {
		return errors.New("balance overflow")
	}
	return putBalance(ctx, account, bal+amount)
}

func balance(ctx contractapi.TransactionContextInterface, account string) (int64, error) {
	b, err := ctx.GetStub().GetState(prefixBal + account)
	if err != nil {
		return 0, err
	}
	if b == nil {
		return 0, nil
	}
	return strconv.ParseInt(string(b), 10, 64)
}

func putBalance(ctx contractapi.TransactionContextInterface, account string, v int64) error {
	return ctx.GetStub().PutState(prefixBal+account, []byte(strconv.FormatInt(v, 10)))
}
