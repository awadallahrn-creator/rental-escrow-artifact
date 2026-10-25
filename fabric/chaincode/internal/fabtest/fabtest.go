// Package fabtest is a small in-memory Fabric ledger used by the chaincode unit tests.
//
// It reproduces the Fabric semantics the chaincode relies on:
//   - reads see only committed state (a transaction never reads its own writes);
//   - a transaction that returns an error is discarded (no partial writes);
//   - chaincode-to-chaincode calls share the caller's transaction (same creator,
//     same timestamp, same signed proposal naming the top-level chaincode);
//   - the creator is a serialized X.509 identity, optionally with Fabric CA attributes
//     (extension 1.2.3.4.5.6.7.8.1), so cid.GetID / cid.GetAttributeValue behave as on a peer.
package fabtest

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/asn1"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"math/big"
	"strconv"
	"time"

	"github.com/hyperledger/fabric-chaincode-go/v2/shim"
	"github.com/hyperledger/fabric-contract-api-go/v2/contractapi"
	"github.com/hyperledger/fabric-protos-go-apiv2/msp"
	"github.com/hyperledger/fabric-protos-go-apiv2/peer"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

var attrOID = asn1.ObjectIdentifier{1, 2, 3, 4, 5, 6, 7, 8, 1}

// Identity is a serialized Fabric identity.
type Identity struct {
	Name       string
	Serialized []byte
}

// NewIdentity creates an identity with an X.509 certificate carrying the given CA attributes.
func NewIdentity(name, mspID string, attrs map[string]string) Identity {
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()),
		Subject:      pkix.Name{CommonName: name, Organization: []string{mspID}},
		Issuer:       pkix.Name{CommonName: "ca." + mspID},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * 365 * time.Hour),
	}
	if attrs != nil {
		b, _ := json.Marshal(map[string]any{"attrs": attrs})
		tmpl.ExtraExtensions = []pkix.Extension{{Id: attrOID, Value: b}}
	}
	der, _ := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	sid, _ := proto.Marshal(&msp.SerializedIdentity{Mspid: mspID, IdBytes: pemBytes})
	return Identity{Name: name, Serialized: sid}
}

// Ledger holds committed state and the registered chaincodes.
type Ledger struct {
	State      map[string][]byte
	chaincodes map[string]*contractapi.ContractChaincode
	txCounter  int
}

// NewLedger creates an empty ledger.
func NewLedger() *Ledger {
	return &Ledger{State: map[string][]byte{}, chaincodes: map[string]*contractapi.ContractChaincode{}}
}

// Register makes a chaincode callable by name.
func (l *Ledger) Register(name string, cc *contractapi.ContractChaincode) { l.chaincodes[name] = cc }

// Result of a transaction.
type Result struct {
	Status  int32
	Message string
	Payload []byte
	Events  []string
}

// OK reports whether the transaction succeeded.
func (r Result) OK() bool { return r.Status == shim.OK }

// Invoke runs one transaction against chaincode `cc` as `who` at unix time `now`.
// Writes are committed only if the transaction succeeds.
func (l *Ledger) Invoke(cc string, who Identity, now int64, fn string, args ...string) Result {
	l.txCounter++
	tx := &txState{
		ledger: l, creator: who.Serialized, now: now, topLevel: cc,
		txID: "tx" + strconv.Itoa(l.txCounter), writes: map[string][]byte{}, deletes: map[string]bool{},
	}
	argv := [][]byte{[]byte(fn)}
	for _, a := range args {
		argv = append(argv, []byte(a))
	}
	resp := tx.invoke(cc, argv)
	if resp.Status == shim.OK {
		for k, v := range tx.writes {
			l.State[k] = v
		}
		for k := range tx.deletes {
			delete(l.State, k)
		}
	}
	return Result{Status: resp.Status, Message: resp.Message, Payload: resp.Payload, Events: tx.events}
}

type txState struct {
	ledger   *Ledger
	creator  []byte
	now      int64
	topLevel string
	txID     string
	writes   map[string][]byte
	deletes  map[string]bool
	events   []string
}

func (tx *txState) invoke(cc string, argv [][]byte) *peer.Response {
	code, ok := tx.ledger.chaincodes[cc]
	if !ok {
		return &peer.Response{Status: shim.ERROR, Message: "chaincode not found: " + cc}
	}
	stub := &stub{tx: tx, ns: cc, args: argv}
	r := code.Invoke(stub)
	return r
}

// stub implements the subset of shim.ChaincodeStubInterface used by the chaincodes.
type stub struct {
	shim.ChaincodeStubInterface
	tx   *txState
	ns   string
	args [][]byte
}

func (s *stub) key(k string) string { return s.ns + "\x00" + k }

func (s *stub) GetArgs() [][]byte { return s.args }

func (s *stub) GetStringArgs() []string {
	out := make([]string, len(s.args))
	for i, a := range s.args {
		out[i] = string(a)
	}
	return out
}

func (s *stub) GetFunctionAndParameters() (string, []string) {
	a := s.GetStringArgs()
	if len(a) == 0 {
		return "", nil
	}
	return a[0], a[1:]
}

func (s *stub) GetTxID() string      { return s.tx.txID }
func (s *stub) GetChannelID() string { return "rentalchannel" }

// GetState returns committed state only (Fabric does not expose uncommitted writes).
func (s *stub) GetState(k string) ([]byte, error) {
	v := s.tx.ledger.State[s.key(k)]
	if v == nil {
		return nil, nil
	}
	return append([]byte(nil), v...), nil
}

func (s *stub) PutState(k string, v []byte) error {
	if k == "" {
		return fmt.Errorf("empty key")
	}
	s.tx.writes[s.key(k)] = append([]byte(nil), v...)
	delete(s.tx.deletes, s.key(k))
	return nil
}

func (s *stub) DelState(k string) error {
	s.tx.deletes[s.key(k)] = true
	delete(s.tx.writes, s.key(k))
	return nil
}

func (s *stub) GetCreator() ([]byte, error) { return s.tx.creator, nil }

func (s *stub) GetTxTimestamp() (*timestamppb.Timestamp, error) {
	return &timestamppb.Timestamp{Seconds: s.tx.now}, nil
}

func (s *stub) SetEvent(name string, _ []byte) error {
	s.tx.events = append(s.tx.events, name)
	return nil
}

func (s *stub) GetSignedProposal() (*peer.SignedProposal, error) {
	spec := &peer.ChaincodeInvocationSpec{ChaincodeSpec: &peer.ChaincodeSpec{
		ChaincodeId: &peer.ChaincodeID{Name: s.tx.topLevel},
	}}
	input, _ := proto.Marshal(spec)
	payload, _ := proto.Marshal(&peer.ChaincodeProposalPayload{Input: input})
	prop, _ := proto.Marshal(&peer.Proposal{Payload: payload})
	return &peer.SignedProposal{ProposalBytes: prop}, nil
}

// InvokeChaincode runs the callee inside the same transaction. Its writes join the caller's
// write set; if it fails, the caller sees the error (and the whole transaction is discarded
// when the caller propagates it).
func (s *stub) InvokeChaincode(name string, args [][]byte, _ string) *peer.Response {
	return s.tx.invoke(name, args)
}
