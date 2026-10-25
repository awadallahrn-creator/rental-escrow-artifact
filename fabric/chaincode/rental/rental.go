// Package rental implements the lease protocol on Hyperledger Fabric with the same state
// machine, arithmetic and deadlines as the Solidity RentalAgreement contract.
//
// Platform mapping (Ethereum -> Fabric):
//   - lease contract instance          -> lease record keyed by lease ID
//   - IdentityRegistry trusted issuer  -> certificate authority of a channel member MSP
//   - attestation expiry / revocation  -> certificate validity period / MSP CRL (enforced by the peer)
//   - attestation role, DID, property  -> X.509 certificate attributes rental.role, rental.did, rental.property
//   - fixed ERC-20 token address       -> fixed token chaincode name in the lease terms
//   - block.timestamp                  -> transaction timestamp (see ARCHITECTURAL NOTE below)
//
// ARCHITECTURAL NOTE: on Fabric the transaction timestamp is chosen by the submitting client and
// is identical for all endorsers. Endorsing peers accept a proposal only if this timestamp is within
// peer.authentication.timewindow of their own clock (TimeWindowCheck auth filter; 15 minutes in the
// Fabric 3.1 release configuration). Deadline checks are therefore precise to within that window,
// whereas on Ethereum block timestamps are produced by the block proposer under consensus rules.
// Lease windows are measured in days, so the window is adequate; the filter must be enabled.
package rental

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strconv"

	"github.com/hyperledger/fabric-chaincode-go/v2/pkg/cid"
	"github.com/hyperledger/fabric-contract-api-go/v2/contractapi"
)

// Lease states (same order as the Solidity enum).
const (
	StateCreated = iota
	StateActive
	StateSettlementProposed
	StateDisputed
	StateSettled
)

// Attribute names carried in the client certificate.
const (
	AttrRole     = "rental.role"
	AttrDID      = "rental.did"
	AttrProperty = "rental.property"
)

// Upper bounds mirroring the Solidity types (uint32 period, uint16 periods, uint32 windows).
const (
	maxPeriodLength = math.MaxUint32
	maxNumPeriods   = math.MaxUint16
	maxWindow       = math.MaxUint32
)

// Terms are fixed at creation.
type Terms struct {
	Landlord          string `json:"landlord"`
	Tenant            string `json:"tenant"`
	Arbitrator        string `json:"arbitrator"`
	TokenChaincode    string `json:"tokenChaincode"`
	PropertyID        string `json:"propertyId"`
	RentAmount        int64  `json:"rentAmount"`
	DepositAmount     int64  `json:"depositAmount"`
	PeriodLength      int64  `json:"periodLength"`
	NumPeriods        int64  `json:"numPeriods"`
	ProposalWindow    int64  `json:"proposalWindow"`
	ChallengeWindow   int64  `json:"challengeWindow"`
	ArbitrationWindow int64  `json:"arbitrationWindow"`
}

// Lease is the full on-ledger record.
type Lease struct {
	ID                   string `json:"id"`
	Terms                Terms  `json:"terms"`
	State                int    `json:"state"`
	LandlordDID          string `json:"landlordDid"`
	TenantDID            string `json:"tenantDid"`
	LeaseStart           int64  `json:"leaseStart"`
	LeaseEnd             int64  `json:"leaseEnd"`
	PeriodsPaid          int64  `json:"periodsPaid"`
	DepositHeld          int64  `json:"depositHeld"`
	DamageClaim          int64  `json:"damageClaim"`
	LandlordEvidence     string `json:"landlordEvidence"`
	TenantEvidence       string `json:"tenantEvidence"`
	ChallengeDeadline    int64  `json:"challengeDeadline"`
	ArbitrationDeadline  int64  `json:"arbitrationDeadline"`
	PaidToLandlord       int64  `json:"paidToLandlord"`
	RefundedToTenant     int64  `json:"refundedToTenant"`
	WithdrawableLandlord int64  `json:"withdrawableLandlord"`
	WithdrawableTenant   int64  `json:"withdrawableTenant"`
}

// Contract is the rental chaincode.
type Contract struct {
	contractapi.Contract
	// OperatorName is this chaincode's own name; it determines the token escrow account.
	OperatorName string
}

// Errors mirroring the Solidity custom errors.
var (
	ErrInvalidTerms     = errors.New("InvalidTerms")
	ErrUnauthorized     = errors.New("Unauthorized")
	ErrIdentityMismatch = errors.New("IdentityMismatch")
	ErrIdentityNotBound = errors.New("IdentityNotBound")
	ErrTooEarly         = errors.New("TooEarly")
	ErrTooLate          = errors.New("TooLate")
	ErrLeaseFullyPaid   = errors.New("LeaseFullyPaid")
	ErrClaimExceeds     = errors.New("ClaimExceedsDeposit")
	ErrMissingEvidence  = errors.New("MissingEvidence")
	ErrAwardExceeds     = errors.New("AwardExceedsClaim")
	ErrNothingWithdraw  = errors.New("NothingToWithdraw")
	ErrLeaseExists      = errors.New("LeaseExists")
	ErrLeaseNotFound    = errors.New("LeaseNotFound")
)

func wrongState(expected, actual int) error {
	return fmt.Errorf("WrongState(expected=%d, actual=%d)", expected, actual)
}

// ===================================================================== creation
// CreateLease records immutable terms. Like contract deployment on Ethereum, anyone may create
// a lease; it has no effect until both named parties bind their identities.
func (c *Contract) CreateLease(ctx contractapi.TransactionContextInterface, id string, termsJSON string) error {
	var t Terms
	if err := json.Unmarshal([]byte(termsJSON), &t); err != nil {
		return ErrInvalidTerms
	}
	if id == "" || t.Landlord == "" || t.Tenant == "" || t.Arbitrator == "" || t.TokenChaincode == "" ||
		t.Landlord == t.Tenant || t.Arbitrator == t.Landlord || t.Arbitrator == t.Tenant || t.PropertyID == "" ||
		t.RentAmount <= 0 || t.DepositAmount <= 0 ||
		t.PeriodLength <= 0 || t.PeriodLength > maxPeriodLength ||
		t.NumPeriods <= 0 || t.NumPeriods > maxNumPeriods ||
		t.ProposalWindow <= 0 || t.ProposalWindow > maxWindow ||
		t.ChallengeWindow <= 0 || t.ChallengeWindow > maxWindow ||
		t.ArbitrationWindow <= 0 || t.ArbitrationWindow > maxWindow {
		return ErrInvalidTerms
	}
	existing, err := ctx.GetStub().GetState(leaseKey(id))
	if err != nil {
		return err
	}
	if existing != nil {
		return ErrLeaseExists
	}
	return c.put(ctx, &Lease{ID: id, Terms: t, State: StateCreated})
}

// ===================================================================== phase 1-2: identity binding
// BindIdentity binds the caller to the lease using certificate attributes issued by a channel
// member CA. Certificate chain, expiry and revocation are verified by the peer's MSP before the
// chaincode runs; the chaincode checks role, DID and (for landlords) the property attribute.
func (c *Contract) BindIdentity(ctx contractapi.TransactionContextInterface, id string) error {
	l, err := c.get(ctx, id)
	if err != nil {
		return err
	}
	if l.State != StateCreated {
		return wrongState(StateCreated, l.State)
	}
	caller, err := cid.GetID(ctx.GetStub())
	if err != nil {
		return err
	}
	role, okRole, err := cid.GetAttributeValue(ctx.GetStub(), AttrRole)
	if err != nil {
		return err
	}
	did, okDID, err := cid.GetAttributeValue(ctx.GetStub(), AttrDID)
	if err != nil {
		return err
	}
	if !okRole || !okDID || did == "" {
		return ErrIdentityMismatch
	}
	switch caller {
	case l.Terms.Landlord:
		prop, okProp, err := cid.GetAttributeValue(ctx.GetStub(), AttrProperty)
		if err != nil {
			return err
		}
		if role != "landlord" || !okProp || prop != l.Terms.PropertyID {
			return ErrIdentityMismatch
		}
		l.LandlordDID = did
	case l.Terms.Tenant:
		if role != "tenant" {
			return ErrIdentityMismatch
		}
		l.TenantDID = did
	default:
		return ErrUnauthorized
	}
	if err := c.put(ctx, l); err != nil {
		return err
	}
	return emit(ctx, "IdentityBound", map[string]any{"lease": id, "role": role, "didHash": did})
}

// ===================================================================== phase 3: deposit locking
func (c *Contract) LockDeposit(ctx contractapi.TransactionContextInterface, id string) error {
	l, err := c.get(ctx, id)
	if err != nil {
		return err
	}
	if err := requireCaller(ctx, l.Terms.Tenant); err != nil {
		return err
	}
	if l.State != StateCreated {
		return wrongState(StateCreated, l.State)
	}
	if l.LandlordDID == "" || l.TenantDID == "" {
		return ErrIdentityNotBound
	}
	now, err := txTime(ctx)
	if err != nil {
		return err
	}
	l.State = StateActive
	l.LeaseStart = now
	l.LeaseEnd = now + l.Terms.PeriodLength*l.Terms.NumPeriods
	l.DepositHeld = l.Terms.DepositAmount
	if err := c.put(ctx, l); err != nil {
		return err
	}
	if err := c.invokeToken(ctx, l, "Transfer", c.escrow(id), l.Terms.DepositAmount); err != nil {
		return err
	}
	return emit(ctx, "DepositLocked", map[string]any{"lease": id, "amount": l.Terms.DepositAmount,
		"leaseStart": l.LeaseStart, "leaseEnd": l.LeaseEnd})
}

// ===================================================================== phase 4: rent execution
// PayRent pays the next unpaid period, at most one period ahead of its due date.
func (c *Contract) PayRent(ctx contractapi.TransactionContextInterface, id string) error {
	l, err := c.get(ctx, id)
	if err != nil {
		return err
	}
	if err := requireCaller(ctx, l.Terms.Tenant); err != nil {
		return err
	}
	if l.State != StateActive {
		return wrongState(StateActive, l.State)
	}
	now, err := txTime(ctx)
	if err != nil {
		return err
	}
	if now >= l.LeaseEnd {
		return ErrTooLate
	}
	period := l.PeriodsPaid
	if period >= l.Terms.NumPeriods {
		return ErrLeaseFullyPaid
	}
	due := l.dueDate(period)
	if now+l.Terms.PeriodLength < due {
		return ErrTooEarly
	}
	l.PeriodsPaid = period + 1
	if err := c.put(ctx, l); err != nil {
		return err
	}
	if err := ctx.GetStub().PutState(paidAtKey(id, period), []byte(strconv.FormatInt(now, 10))); err != nil {
		return err
	}
	// rent flows directly tenant -> landlord
	if err := c.invokeToken(ctx, l, "Transfer", l.Terms.Landlord, l.Terms.RentAmount); err != nil {
		return err
	}
	return emit(ctx, "RentPaid", map[string]any{"lease": id, "period": period, "amount": l.Terms.RentAmount,
		"dueAt": due, "paidAt": now})
}

// ===================================================================== phase 5: settlement
func (c *Contract) ProposeSettlement(ctx contractapi.TransactionContextInterface, id string, claim int64, evidence string) error {
	l, err := c.get(ctx, id)
	if err != nil {
		return err
	}
	if err := requireCaller(ctx, l.Terms.Landlord); err != nil {
		return err
	}
	if l.State != StateActive {
		return wrongState(StateActive, l.State)
	}
	now, err := txTime(ctx)
	if err != nil {
		return err
	}
	if now < l.LeaseEnd {
		return ErrTooEarly
	}
	if now > l.LeaseEnd+l.Terms.ProposalWindow {
		return ErrTooLate
	}
	if claim < 0 || claim > l.Terms.DepositAmount {
		return ErrClaimExceeds
	}
	if claim == 0 {
		return c.settle(ctx, l, 0, now)
	}
	if evidence == "" {
		return ErrMissingEvidence
	}
	l.DamageClaim = claim
	l.LandlordEvidence = evidence
	l.ChallengeDeadline = now + l.Terms.ChallengeWindow
	l.State = StateSettlementProposed
	if err := c.put(ctx, l); err != nil {
		return err
	}
	return emit(ctx, "SettlementProposed", map[string]any{"lease": id, "damageClaim": claim, "evidence": evidence,
		"challengeDeadline": l.ChallengeDeadline})
}

func (c *Contract) AcceptSettlement(ctx contractapi.TransactionContextInterface, id string) error {
	l, err := c.get(ctx, id)
	if err != nil {
		return err
	}
	if err := requireCaller(ctx, l.Terms.Tenant); err != nil {
		return err
	}
	if l.State != StateSettlementProposed {
		return wrongState(StateSettlementProposed, l.State)
	}
	now, err := txTime(ctx)
	if err != nil {
		return err
	}
	return c.settle(ctx, l, l.DamageClaim, now)
}

func (c *Contract) DisputeSettlement(ctx contractapi.TransactionContextInterface, id string, evidence string) error {
	l, err := c.get(ctx, id)
	if err != nil {
		return err
	}
	if err := requireCaller(ctx, l.Terms.Tenant); err != nil {
		return err
	}
	if l.State != StateSettlementProposed {
		return wrongState(StateSettlementProposed, l.State)
	}
	now, err := txTime(ctx)
	if err != nil {
		return err
	}
	if now > l.ChallengeDeadline {
		return ErrTooLate
	}
	if evidence == "" {
		return ErrMissingEvidence
	}
	l.TenantEvidence = evidence
	l.ArbitrationDeadline = now + l.Terms.ArbitrationWindow
	l.State = StateDisputed
	if err := c.put(ctx, l); err != nil {
		return err
	}
	return emit(ctx, "SettlementDisputed", map[string]any{"lease": id, "evidence": evidence,
		"arbitrationDeadline": l.ArbitrationDeadline})
}

func (c *Contract) FinalizeSettlement(ctx contractapi.TransactionContextInterface, id string) error {
	l, err := c.get(ctx, id)
	if err != nil {
		return err
	}
	if l.State != StateSettlementProposed {
		return wrongState(StateSettlementProposed, l.State)
	}
	now, err := txTime(ctx)
	if err != nil {
		return err
	}
	if now <= l.ChallengeDeadline {
		return ErrTooEarly
	}
	return c.settle(ctx, l, l.DamageClaim, now)
}

func (c *Contract) ResolveDispute(ctx contractapi.TransactionContextInterface, id string, award int64) error {
	l, err := c.get(ctx, id)
	if err != nil {
		return err
	}
	if l.State != StateDisputed {
		return wrongState(StateDisputed, l.State)
	}
	if err := requireCaller(ctx, l.Terms.Arbitrator); err != nil {
		return err
	}
	now, err := txTime(ctx)
	if err != nil {
		return err
	}
	if now > l.ArbitrationDeadline {
		return ErrTooLate
	}
	if award < 0 || award > l.DamageClaim {
		return ErrAwardExceeds
	}
	return c.settle(ctx, l, award, now)
}

func (c *Contract) ClaimAfterTimeout(ctx contractapi.TransactionContextInterface, id string) error {
	l, err := c.get(ctx, id)
	if err != nil {
		return err
	}
	now, err := txTime(ctx)
	if err != nil {
		return err
	}
	switch l.State {
	case StateActive:
		if now <= l.LeaseEnd+l.Terms.ProposalWindow {
			return ErrTooEarly
		}
	case StateDisputed:
		if now <= l.ArbitrationDeadline {
			return ErrTooEarly
		}
	default:
		return wrongState(StateActive, l.State)
	}
	return c.settle(ctx, l, 0, now)
}

// Withdraw transfers the caller's allocated share out of escrow (pull pattern).
func (c *Contract) Withdraw(ctx contractapi.TransactionContextInterface, id string) error {
	l, err := c.get(ctx, id)
	if err != nil {
		return err
	}
	caller, err := cid.GetID(ctx.GetStub())
	if err != nil {
		return err
	}
	var amount int64
	switch caller {
	case l.Terms.Landlord:
		amount, l.WithdrawableLandlord = l.WithdrawableLandlord, 0
	case l.Terms.Tenant:
		amount, l.WithdrawableTenant = l.WithdrawableTenant, 0
	}
	if amount == 0 {
		return ErrNothingWithdraw
	}
	if err := c.put(ctx, l); err != nil {
		return err
	}
	if err := c.invokeToken(ctx, l, "ReleaseEscrow", c.escrow(id), caller, amount); err != nil {
		return err
	}
	return emit(ctx, "Withdrawn", map[string]any{"lease": id, "amount": amount})
}

// ===================================================================== queries
func (c *Contract) GetLease(ctx contractapi.TransactionContextInterface, id string) (*Lease, error) {
	return c.get(ctx, id)
}

// Arrears returns the number of due but unpaid periods at the transaction time.
func (c *Contract) Arrears(ctx contractapi.TransactionContextInterface, id string) (int64, error) {
	l, err := c.get(ctx, id)
	if err != nil {
		return 0, err
	}
	now, err := txTime(ctx)
	if err != nil {
		return 0, err
	}
	return l.arrears(now), nil
}

// ===================================================================== internal
func (l *Lease) dueDate(period int64) int64 { return l.LeaseStart + period*l.Terms.PeriodLength }

func (l *Lease) periodsDue(now int64) int64 {
	if l.State == StateCreated {
		return 0
	}
	if now >= l.LeaseEnd {
		return l.Terms.NumPeriods
	}
	return (now-l.LeaseStart)/l.Terms.PeriodLength + 1
}

func (l *Lease) arrears(now int64) int64 {
	due := l.periodsDue(now)
	if due > l.PeriodsPaid {
		return due - l.PeriodsPaid
	}
	return 0
}

// settle mirrors RentalAgreement._settle: objective unpaid rent first, then damage, capped at
// the deposit; shares are allocated for later withdrawal.
func (c *Contract) settle(ctx contractapi.TransactionContextInterface, l *Lease, damage int64, now int64) error {
	held := l.DepositHeld
	unpaid := l.arrears(now) * l.Terms.RentAmount
	unpaidPortion := min(unpaid, held)
	remaining := held - unpaidPortion
	damagePortion := min(damage, remaining)
	toLandlord := unpaidPortion + damagePortion
	toTenant := held - toLandlord

	l.State = StateSettled
	l.DepositHeld = 0
	l.PaidToLandlord = toLandlord
	l.RefundedToTenant = toTenant
	l.WithdrawableLandlord += toLandlord
	l.WithdrawableTenant += toTenant
	if err := c.put(ctx, l); err != nil {
		return err
	}
	return emit(ctx, "DepositSettled", map[string]any{"lease": l.ID, "toLandlord": toLandlord,
		"toTenant": toTenant, "unpaidRentPortion": unpaidPortion, "damagePortion": damagePortion})
}

// escrow is the per-lease escrow account in the token chaincode.
func (c *Contract) escrow(id string) string { return "escrow:" + c.OperatorName + ":" + id }

// invokeToken calls the lease's token chaincode in the same transaction; args precede the amount.
func (c *Contract) invokeToken(ctx contractapi.TransactionContextInterface, l *Lease, fn string, args ...any) error {
	argv := [][]byte{[]byte(fn)}
	for _, a := range args {
		switch v := a.(type) {
		case string:
			argv = append(argv, []byte(v))
		case int64:
			argv = append(argv, []byte(strconv.FormatInt(v, 10)))
		}
	}
	resp := ctx.GetStub().InvokeChaincode(l.Terms.TokenChaincode, argv, "")
	if resp.Status != 200 {
		return fmt.Errorf("token %s failed: %s", fn, resp.Message)
	}
	return nil
}

func (c *Contract) get(ctx contractapi.TransactionContextInterface, id string) (*Lease, error) {
	b, err := ctx.GetStub().GetState(leaseKey(id))
	if err != nil {
		return nil, err
	}
	if b == nil {
		return nil, ErrLeaseNotFound
	}
	var l Lease
	if err := json.Unmarshal(b, &l); err != nil {
		return nil, err
	}
	return &l, nil
}

func (c *Contract) put(ctx contractapi.TransactionContextInterface, l *Lease) error {
	b, err := json.Marshal(l)
	if err != nil {
		return err
	}
	return ctx.GetStub().PutState(leaseKey(l.ID), b)
}

func requireCaller(ctx contractapi.TransactionContextInterface, expected string) error {
	caller, err := cid.GetID(ctx.GetStub())
	if err != nil {
		return err
	}
	if caller != expected {
		return ErrUnauthorized
	}
	return nil
}

func txTime(ctx contractapi.TransactionContextInterface) (int64, error) {
	ts, err := ctx.GetStub().GetTxTimestamp()
	if err != nil {
		return 0, err
	}
	return ts.Seconds, nil
}

func emit(ctx contractapi.TransactionContextInterface, name string, payload map[string]any) error {
	b, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	return ctx.GetStub().SetEvent(name, b)
}

func leaseKey(id string) string             { return "lease:" + id }
func paidAtKey(id string, p int64) string   { return "paid:" + id + ":" + strconv.FormatInt(p, 10) }
