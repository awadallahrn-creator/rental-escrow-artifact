package rental_test

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math/rand"
	"strconv"
	"strings"
	"testing"

	"github.com/hyperledger/fabric-contract-api-go/v2/contractapi"
	"github.com/rental-escrow/chaincode/internal/fabtest"
	"github.com/rental-escrow/chaincode/rental"
	"github.com/rental-escrow/chaincode/token"
)

// Same parameters as the Solidity test fixture.
const (
	rent       = 500_000_000   // 500 units, 6 decimals
	deposit    = 1_000_000_000 // 1,000 units
	period     = 30 * 86400
	nPeriods   = 12
	propWin    = 14 * 86400
	chalWin    = 14 * 86400
	arbWin     = 30 * 86400
	property   = "did:example:property:ramallah-unit-12"
	landlordID = "did:example:landlord-001"
	tenantID   = "did:example:tenant-001"
	t0         = int64(1_760_000_000)
)

type env struct {
	t                                           *testing.T
	l                                           *fabtest.Ledger
	minter, landlord, tenant, arbitrator, other fabtest.Identity
	now                                         int64
}

// clientID reproduces cid.GetID for an identity: base64("x509::<subject>::<issuer>").
func clientID(t *testing.T, l *fabtest.Ledger, who fabtest.Identity) string {
	r := l.Invoke("token", who, 0, "ClientAccountID")
	if !r.OK() {
		t.Fatalf("ClientAccountID: %s", r.Message)
	}
	return string(r.Payload)
}

func newEnv(t *testing.T) *env {
	l := fabtest.NewLedger()
	tcc, err := contractapi.NewChaincode(&token.Contract{})
	if err != nil {
		t.Fatal(err)
	}
	rcc, err := contractapi.NewChaincode(&rental.Contract{OperatorName: "rental"})
	if err != nil {
		t.Fatal(err)
	}
	l.Register("token", tcc)
	l.Register("rental", rcc)
	e := &env{
		t: t, l: l, now: t0,
		minter:     fabtest.NewIdentity("minter", "Org1MSP", nil),
		landlord:   fabtest.NewIdentity("landlord", "Org1MSP", map[string]string{rental.AttrRole: "landlord", rental.AttrDID: landlordID, rental.AttrProperty: property}),
		tenant:     fabtest.NewIdentity("tenant", "Org2MSP", map[string]string{rental.AttrRole: "tenant", rental.AttrDID: tenantID}),
		arbitrator: fabtest.NewIdentity("arbitrator", "Org3MSP", nil),
		other:      fabtest.NewIdentity("stranger", "Org3MSP", map[string]string{rental.AttrRole: "tenant", rental.AttrDID: "did:example:x"}),
	}
	e.must(l.Invoke("token", e.minter, e.now, "Initialize", "rental"))
	e.must(l.Invoke("token", e.minter, e.now, "Mint", e.id(e.tenant), strconv.Itoa(100_000_000_000)))
	e.must(l.Invoke("rental", e.landlord, e.now, "CreateLease", "L1", e.termsJSON(nil)))
	return e
}

func (e *env) id(who fabtest.Identity) string { return clientID(e.t, e.l, who) }

func (e *env) termsJSON(mod func(*rental.Terms)) string {
	tm := rental.Terms{
		Landlord: e.id(e.landlord), Tenant: e.id(e.tenant), Arbitrator: e.id(e.arbitrator),
		TokenChaincode: "token", PropertyID: property, RentAmount: rent, DepositAmount: deposit,
		PeriodLength: period, NumPeriods: nPeriods, ProposalWindow: propWin, ChallengeWindow: chalWin,
		ArbitrationWindow: arbWin,
	}
	if mod != nil {
		mod(&tm)
	}
	b, _ := json.Marshal(tm)
	return string(b)
}

func (e *env) call(who fabtest.Identity, fn string, args ...string) fabtest.Result {
	return e.l.Invoke("rental", who, e.now, fn, append([]string{"L1"}, args...)...)
}

func (e *env) must(r fabtest.Result) fabtest.Result {
	e.t.Helper()
	if !r.OK() {
		e.t.Fatalf("unexpected failure: %s", r.Message)
	}
	return r
}

func (e *env) fails(r fabtest.Result, want string) {
	e.t.Helper()
	if r.OK() {
		e.t.Fatalf("expected failure %q, got success", want)
	}
	if !strings.Contains(r.Message, want) {
		e.t.Fatalf("expected error containing %q, got %q", want, r.Message)
	}
}

func (e *env) lease() rental.Lease {
	r := e.must(e.l.Invoke("rental", e.minter, e.now, "GetLease", "L1"))
	var l rental.Lease
	if err := json.Unmarshal(r.Payload, &l); err != nil {
		e.t.Fatal(err)
	}
	return l
}

func (e *env) bal(who string) int64 {
	r := e.must(e.l.Invoke("token", e.minter, e.now, "BalanceOf", who))
	v, _ := strconv.ParseInt(string(r.Payload), 10, 64)
	return v
}

func (e *env) activate() {
	e.must(e.call(e.landlord, "BindIdentity"))
	e.must(e.call(e.tenant, "BindIdentity"))
	e.must(e.call(e.tenant, "LockDeposit"))
}

func (e *env) payPeriods(n int) {
	start := e.lease().LeaseStart
	for i := 0; i < n; i++ {
		e.now = start + int64(i)*period
		e.must(e.call(e.tenant, "PayRent"))
	}
}

func (e *env) toLeaseEnd() { e.now = e.lease().LeaseEnd }

func (e *env) withdrawAll() {
	l := e.lease()
	if l.WithdrawableLandlord > 0 {
		e.must(e.call(e.landlord, "Withdraw"))
	}
	if l.WithdrawableTenant > 0 {
		e.must(e.call(e.tenant, "Withdraw"))
	}
}

const escrow = "escrow:rental:L1"

// ------------------------------------------------------------------ identity binding
func TestBindBothParties(t *testing.T) {
	e := newEnv(t)
	e.must(e.call(e.landlord, "BindIdentity"))
	e.must(e.call(e.tenant, "BindIdentity"))
	l := e.lease()
	if l.LandlordDID != landlordID || l.TenantDID != tenantID {
		t.Fatalf("DIDs not bound: %+v", l)
	}
}

func TestLockWithoutIdentityFails(t *testing.T) {
	e := newEnv(t)
	e.must(e.call(e.landlord, "BindIdentity"))
	e.fails(e.call(e.tenant, "LockDeposit"), "IdentityNotBound")
}

func TestWrongRoleFails(t *testing.T) {
	e := newEnv(t)
	// landlord identity carrying the tenant role
	e.landlord = fabtest.NewIdentity("landlord", "Org1MSP", map[string]string{rental.AttrRole: "tenant", rental.AttrDID: landlordID, rental.AttrProperty: property})
	e.must(e.l.Invoke("rental", e.landlord, e.now, "CreateLease", "L2", e.termsJSON(nil)))
	e.fails(e.l.Invoke("rental", e.landlord, e.now, "BindIdentity", "L2"), "IdentityMismatch")
}

func TestLandlordAttestedForOtherPropertyFails(t *testing.T) {
	e := newEnv(t)
	e.landlord = fabtest.NewIdentity("landlord", "Org1MSP", map[string]string{rental.AttrRole: "landlord", rental.AttrDID: landlordID, rental.AttrProperty: "other-unit"})
	e.must(e.l.Invoke("rental", e.landlord, e.now, "CreateLease", "L2", e.termsJSON(nil)))
	e.fails(e.l.Invoke("rental", e.landlord, e.now, "BindIdentity", "L2"), "IdentityMismatch")
}

func TestMissingAttributesFail(t *testing.T) {
	e := newEnv(t)
	e.tenant = fabtest.NewIdentity("tenant", "Org2MSP", nil)
	e.must(e.l.Invoke("rental", e.landlord, e.now, "CreateLease", "L2", e.termsJSON(nil)))
	e.fails(e.l.Invoke("rental", e.tenant, e.now, "BindIdentity", "L2"), "IdentityMismatch")
}

func TestThirdPartyBindFails(t *testing.T) {
	e := newEnv(t)
	e.fails(e.call(e.other, "BindIdentity"), "Unauthorized")
}

func TestBindAfterActivationFails(t *testing.T) {
	e := newEnv(t)
	e.activate()
	e.fails(e.call(e.tenant, "BindIdentity"), "WrongState")
}

func TestInvalidTermsAndDuplicateLease(t *testing.T) {
	e := newEnv(t)
	e.fails(e.l.Invoke("rental", e.landlord, e.now, "CreateLease", "L2", e.termsJSON(func(tm *rental.Terms) { tm.Arbitrator = tm.Landlord })), "InvalidTerms")
	e.fails(e.l.Invoke("rental", e.landlord, e.now, "CreateLease", "L2", e.termsJSON(func(tm *rental.Terms) { tm.DepositAmount = 0 })), "InvalidTerms")
	e.fails(e.l.Invoke("rental", e.landlord, e.now, "CreateLease", "L2", e.termsJSON(func(tm *rental.Terms) { tm.NumPeriods = 70000 })), "InvalidTerms")
	e.fails(e.l.Invoke("rental", e.landlord, e.now, "CreateLease", "L1", e.termsJSON(nil)), "LeaseExists")
}

// ------------------------------------------------------------------ deposit and rent
func TestLockDeposit(t *testing.T) {
	e := newEnv(t)
	e.activate()
	l := e.lease()
	if l.State != rental.StateActive || l.DepositHeld != deposit || l.LeaseEnd != l.LeaseStart+period*nPeriods {
		t.Fatalf("bad lease after lock: %+v", l)
	}
	if e.bal(escrow) != deposit {
		t.Fatalf("escrow balance %d", e.bal(escrow))
	}
}

func TestNonTenantLockFails(t *testing.T) {
	e := newEnv(t)
	e.must(e.call(e.landlord, "BindIdentity"))
	e.must(e.call(e.tenant, "BindIdentity"))
	e.fails(e.call(e.landlord, "LockDeposit"), "Unauthorized")
}

func TestLockTwiceFails(t *testing.T) {
	e := newEnv(t)
	e.activate()
	e.fails(e.call(e.tenant, "LockDeposit"), "WrongState")
}

func TestRentGoesDirectlyToLandlord(t *testing.T) {
	e := newEnv(t)
	e.activate()
	e.payPeriods(3)
	if e.bal(e.id(e.landlord)) != 3*rent || e.bal(escrow) != deposit || e.lease().PeriodsPaid != 3 {
		t.Fatal("rent not routed directly to landlord")
	}
}

func TestPayingTwoPeriodsAheadFails(t *testing.T) {
	e := newEnv(t)
	e.activate()
	e.must(e.call(e.tenant, "PayRent"))
	e.must(e.call(e.tenant, "PayRent"))
	e.fails(e.call(e.tenant, "PayRent"), "TooEarly")
}

func TestPayAfterLeaseEndFails(t *testing.T) {
	e := newEnv(t)
	e.activate()
	e.toLeaseEnd()
	e.fails(e.call(e.tenant, "PayRent"), "TooLate")
}

func TestLeaseFullyPaidFails(t *testing.T) {
	e := newEnv(t)
	e.activate()
	e.payPeriods(nPeriods)
	e.fails(e.call(e.tenant, "PayRent"), "LeaseFullyPaid")
}

func TestArrearsTracking(t *testing.T) {
	e := newEnv(t)
	e.activate()
	e.payPeriods(2)
	e.now = e.lease().LeaseStart + 4*period
	r := e.must(e.call(e.minter, "Arrears"))
	if string(r.Payload) != "3" {
		t.Fatalf("arrears = %s, want 3", r.Payload)
	}
}

// Deposits of different leases use different escrow accounts (no shared ledger key).
func TestEscrowAccountsArePerLease(t *testing.T) {
	e := newEnv(t)
	e.activate()
	e.must(e.l.Invoke("rental", e.landlord, e.now, "CreateLease", "L2", e.termsJSON(nil)))
	e.must(e.l.Invoke("rental", e.landlord, e.now, "BindIdentity", "L2"))
	e.must(e.l.Invoke("rental", e.tenant, e.now, "BindIdentity", "L2"))
	e.must(e.l.Invoke("rental", e.tenant, e.now, "LockDeposit", "L2"))
	if e.bal("escrow:rental:L1") != deposit || e.bal("escrow:rental:L2") != deposit {
		t.Fatal("escrow balances not separated per lease")
	}
	// the rental chaincode cannot be tricked into releasing from a non-escrow account
	r := e.l.Invoke("token", e.landlord, e.now, "ReleaseEscrow", e.id(e.tenant), e.id(e.landlord), "1")
	e.fails(r, "unauthorized")
}

func TestFailedTransferRollsBackLease(t *testing.T) {
	e := newEnv(t)
	poor := fabtest.NewIdentity("poor", "Org2MSP", map[string]string{rental.AttrRole: "tenant", rental.AttrDID: "did:example:poor"})
	e.tenant = poor // no tokens minted to this identity
	e.must(e.l.Invoke("rental", e.landlord, e.now, "CreateLease", "L2", e.termsJSON(nil)))
	e.must(e.l.Invoke("rental", e.landlord, e.now, "BindIdentity", "L2"))
	e.must(e.l.Invoke("rental", poor, e.now, "BindIdentity", "L2"))
	e.fails(e.l.Invoke("rental", poor, e.now, "LockDeposit", "L2"), "insufficient balance")
	r := e.must(e.l.Invoke("rental", e.minter, e.now, "GetLease", "L2"))
	var l rental.Lease
	_ = json.Unmarshal(r.Payload, &l)
	if l.State != rental.StateCreated || l.DepositHeld != 0 {
		t.Fatalf("lease state changed despite failed transfer: %+v", l)
	}
}

// ------------------------------------------------------------------ settlement
func endFullyPaid(e *env) {
	e.activate()
	e.payPeriods(nPeriods)
	e.toLeaseEnd()
}

func TestZeroClaimRefundsFullDeposit(t *testing.T) {
	e := newEnv(t)
	endFullyPaid(e)
	before := e.bal(e.id(e.tenant))
	e.must(e.call(e.landlord, "ProposeSettlement", "0", ""))
	if e.lease().State != rental.StateSettled || e.bal(escrow) != deposit {
		t.Fatal("expected settled with funds still in escrow until withdrawal")
	}
	e.must(e.call(e.tenant, "Withdraw"))
	if e.bal(e.id(e.tenant))-before != deposit || e.bal(escrow) != 0 {
		t.Fatal("tenant not refunded")
	}
}

func TestProposeBeforeLeaseEndFails(t *testing.T) {
	e := newEnv(t)
	e.activate()
	e.fails(e.call(e.landlord, "ProposeSettlement", "100000000", "ipfs://r"), "TooEarly")
}

func TestClaimWithoutEvidenceFails(t *testing.T) {
	e := newEnv(t)
	endFullyPaid(e)
	e.fails(e.call(e.landlord, "ProposeSettlement", "100000000", ""), "MissingEvidence")
}

func TestClaimExceedsDepositFails(t *testing.T) {
	e := newEnv(t)
	endFullyPaid(e)
	e.fails(e.call(e.landlord, "ProposeSettlement", strconv.Itoa(deposit+1), "ipfs://r"), "ClaimExceedsDeposit")
}

func TestTenantProposeFails(t *testing.T) {
	e := newEnv(t)
	endFullyPaid(e)
	e.fails(e.call(e.tenant, "ProposeSettlement", "0", ""), "Unauthorized")
}

func TestTenantAcceptsClaim(t *testing.T) {
	e := newEnv(t)
	endFullyPaid(e)
	e.must(e.call(e.landlord, "ProposeSettlement", "300000000", "ipfs://r"))
	e.must(e.call(e.tenant, "AcceptSettlement"))
	if l := e.lease(); l.PaidToLandlord != 300_000_000 || l.RefundedToTenant != 700_000_000 {
		t.Fatalf("bad split %+v", l)
	}
}

func TestUnchallengedClaimFinalizesAfterWindow(t *testing.T) {
	e := newEnv(t)
	endFullyPaid(e)
	e.must(e.call(e.landlord, "ProposeSettlement", "300000000", "ipfs://r"))
	e.fails(e.call(e.other, "FinalizeSettlement"), "TooEarly")
	e.now = e.lease().ChallengeDeadline + 1
	e.must(e.call(e.other, "FinalizeSettlement"))
	if l := e.lease(); l.PaidToLandlord != 300_000_000 || l.RefundedToTenant != 700_000_000 {
		t.Fatalf("bad split %+v", l)
	}
}

func TestDisputeAfterWindowFails(t *testing.T) {
	e := newEnv(t)
	endFullyPaid(e)
	e.must(e.call(e.landlord, "ProposeSettlement", "300000000", "ipfs://r"))
	e.now = e.lease().ChallengeDeadline + 1
	e.fails(e.call(e.tenant, "DisputeSettlement", "ipfs://t"), "TooLate")
}

func TestArbitratorReducesClaim(t *testing.T) {
	e := newEnv(t)
	endFullyPaid(e)
	e.must(e.call(e.landlord, "ProposeSettlement", "600000000", "ipfs://r"))
	e.must(e.call(e.tenant, "DisputeSettlement", "ipfs://t"))
	e.must(e.call(e.arbitrator, "ResolveDispute", "150000000"))
	if l := e.lease(); l.PaidToLandlord != 150_000_000 || l.RefundedToTenant != 850_000_000 {
		t.Fatalf("bad split %+v", l)
	}
}

func TestArbitratorExceedsClaimFails(t *testing.T) {
	e := newEnv(t)
	endFullyPaid(e)
	e.must(e.call(e.landlord, "ProposeSettlement", "200000000", "ipfs://r"))
	e.must(e.call(e.tenant, "DisputeSettlement", "ipfs://t"))
	e.fails(e.call(e.arbitrator, "ResolveDispute", "200000001"), "AwardExceedsClaim")
}

func TestLandlordResolvesOwnDisputeFails(t *testing.T) {
	e := newEnv(t)
	endFullyPaid(e)
	e.must(e.call(e.landlord, "ProposeSettlement", "200000000", "ipfs://r"))
	e.must(e.call(e.tenant, "DisputeSettlement", "ipfs://t"))
	e.fails(e.call(e.landlord, "ResolveDispute", "200000000"), "Unauthorized")
}

func TestInactiveArbitratorDefaultsToTenantOnDamage(t *testing.T) {
	e := newEnv(t)
	endFullyPaid(e)
	e.must(e.call(e.landlord, "ProposeSettlement", "600000000", "ipfs://r"))
	e.must(e.call(e.tenant, "DisputeSettlement", "ipfs://t"))
	e.fails(e.call(e.other, "ClaimAfterTimeout"), "TooEarly")
	e.now = e.lease().ArbitrationDeadline + 1
	e.must(e.call(e.other, "ClaimAfterTimeout"))
	if l := e.lease(); l.PaidToLandlord != 0 || l.RefundedToTenant != deposit {
		t.Fatalf("bad split %+v", l)
	}
}

func TestInactiveLandlordCannotFreezeDeposit(t *testing.T) {
	e := newEnv(t)
	endFullyPaid(e)
	e.now = e.lease().LeaseEnd + propWin + 1
	e.must(e.call(e.tenant, "ClaimAfterTimeout"))
	if e.lease().RefundedToTenant != deposit {
		t.Fatal("tenant not refunded")
	}
	e.fails(e.call(e.landlord, "ProposeSettlement", "100000000", "ipfs://r"), "WrongState")
}

func TestUnpaidRentDeductedWithoutDiscretion(t *testing.T) {
	e := newEnv(t)
	e.activate()
	e.payPeriods(10)
	e.toLeaseEnd()
	e.must(e.call(e.landlord, "ProposeSettlement", "0", ""))
	if l := e.lease(); l.PaidToLandlord != 2*rent || l.RefundedToTenant != deposit-2*rent {
		t.Fatalf("bad split %+v", l)
	}
}

func TestUnpaidRentPlusDamageCappedAtDeposit(t *testing.T) {
	e := newEnv(t)
	e.activate()
	e.payPeriods(11)
	e.toLeaseEnd()
	e.must(e.call(e.landlord, "ProposeSettlement", "800000000", "ipfs://r"))
	e.must(e.call(e.tenant, "AcceptSettlement"))
	if l := e.lease(); l.PaidToLandlord != deposit || l.RefundedToTenant != 0 {
		t.Fatalf("bad split %+v", l)
	}
}

func TestArrearsStillDeductedWhenArbitratorInactive(t *testing.T) {
	e := newEnv(t)
	e.activate()
	e.payPeriods(11)
	e.toLeaseEnd()
	e.must(e.call(e.landlord, "ProposeSettlement", "300000000", "ipfs://r"))
	e.must(e.call(e.tenant, "DisputeSettlement", "ipfs://t"))
	e.now = e.lease().ArbitrationDeadline + 1
	e.must(e.call(e.other, "ClaimAfterTimeout"))
	if l := e.lease(); l.PaidToLandlord != rent || l.RefundedToTenant != deposit-rent {
		t.Fatalf("bad split %+v", l)
	}
}

func TestSettleTwiceFails(t *testing.T) {
	e := newEnv(t)
	endFullyPaid(e)
	e.must(e.call(e.landlord, "ProposeSettlement", "0", ""))
	e.fails(e.call(e.other, "ClaimAfterTimeout"), "WrongState")
	e.fails(e.call(e.tenant, "AcceptSettlement"), "WrongState")
}

// I1 is a solvency bound (balance >= liabilities): token.Transfer accepts any destination, so a
// client can send tokens to a lease's escrow account without a protocol call. Such surplus does
// not change the allocation or block withdrawals, and ReleaseEscrow never releases it.
func TestUnsolicitedTransfersDoNotChangeAllocation(t *testing.T) {
	e := newEnv(t)
	escrow := "escrow:rental:L1"
	e.must(e.l.Invoke("token", e.minter, e.now, "Mint", e.id(e.other), "30000000"))
	donate := func() { e.must(e.l.Invoke("token", e.other, e.now, "Transfer", escrow, "10000000")) }

	donate() // before the deposit is locked (state Created)
	e.activate()
	donate() // during the lease (state Active)
	e.payPeriods(nPeriods)
	e.toLeaseEnd()
	e.must(e.call(e.landlord, "ProposeSettlement", "300000000", "ipfs://r"))
	e.must(e.call(e.tenant, "AcceptSettlement"))
	if l := e.lease(); l.PaidToLandlord != 300_000_000 || l.RefundedToTenant != 700_000_000 {
		t.Fatalf("allocation changed by unsolicited transfers: %+v", l)
	}
	donate() // after settlement (state Settled)
	if e.bal(escrow) != deposit+30_000_000 {
		t.Fatalf("escrow balance %d", e.bal(escrow))
	}

	l0, t0 := e.bal(e.id(e.landlord)), e.bal(e.id(e.tenant))
	e.must(e.call(e.landlord, "Withdraw"))
	e.must(e.call(e.tenant, "Withdraw"))
	if e.bal(e.id(e.landlord))-l0 != 300_000_000 || e.bal(e.id(e.tenant))-t0 != 700_000_000 {
		t.Fatal("withdrawn amounts differ from the allocation")
	}
	if l := e.lease(); l.DepositHeld+l.WithdrawableLandlord+l.WithdrawableTenant != 0 {
		t.Fatalf("liabilities remain: %+v", l)
	}
	if e.bal(escrow) != 30_000_000 {
		t.Fatalf("surplus should remain in escrow, got %d", e.bal(escrow))
	}
}

// L1 bound: each timeout is enabled only strictly after its deadline, and the latest deadline is
// LeaseEnd + proposal + challenge + arbitration window.
func TestTimeoutBoundariesAreStrict(t *testing.T) {
	e := newEnv(t)
	endFullyPaid(e)
	e.now = e.lease().LeaseEnd + propWin // t == proposal deadline
	e.fails(e.call(e.other, "ClaimAfterTimeout"), "TooEarly")
	e.must(e.call(e.landlord, "ProposeSettlement", "300000000", "ipfs://r"))

	e.now = e.lease().ChallengeDeadline // t == challenge deadline
	e.fails(e.call(e.other, "FinalizeSettlement"), "TooEarly")
	e.must(e.call(e.tenant, "DisputeSettlement", "ipfs://t"))

	l := e.lease()
	e.now = l.ArbitrationDeadline // t == arbitration deadline
	e.fails(e.call(e.other, "ClaimAfterTimeout"), "TooEarly")
	if l.ArbitrationDeadline != l.LeaseEnd+propWin+chalWin+arbWin {
		t.Fatalf("latest deadline %d, want %d", l.ArbitrationDeadline, l.LeaseEnd+propWin+chalWin+arbWin)
	}
	e.now++
	e.must(e.call(e.other, "ClaimAfterTimeout"))
	if e.lease().State != rental.StateSettled {
		t.Fatal("lease not settled after the latest deadline")
	}
}

func TestWithdrawNothingAndTwiceFails(t *testing.T) {
	e := newEnv(t)
	e.fails(e.call(e.tenant, "Withdraw"), "NothingToWithdraw")
	endFullyPaid(e)
	e.must(e.call(e.landlord, "ProposeSettlement", "0", ""))
	e.must(e.call(e.tenant, "Withdraw"))
	e.fails(e.call(e.tenant, "Withdraw"), "NothingToWithdraw")
}

// The escrow account can only be drained through the rental chaincode's rules.
func TestEscrowReleaseOnlyThroughRentalChaincode(t *testing.T) {
	e := newEnv(t)
	e.activate()
	r := e.l.Invoke("token", e.landlord, e.now, "ReleaseEscrow", escrow, e.id(e.landlord), strconv.Itoa(deposit))
	e.fails(r, "unauthorized")
	if e.bal(escrow) != deposit {
		t.Fatal("escrow drained directly")
	}
}

// ------------------------------------------------------------------ property: conservation
// Same property as the Solidity fuzz test: for random arrears, claims, awards and each of the
// four settlement routes, the deposit is split exactly as specified and fully paid out.
func TestPropertySettlementConservesDeposit(t *testing.T) {
	rng := rand.New(rand.NewSource(42))
	for run := 0; run < 300; run++ {
		e := newEnv(t)
		paid := rng.Intn(nPeriods + 1)
		claim := int64(1 + rng.Int63n(deposit))
		award := rng.Int63n(claim + 1)
		path := rng.Intn(4)

		e.activate()
		e.payPeriods(paid)
		e.toLeaseEnd()
		unpaid := int64(nPeriods-paid) * rent
		tenantBefore, landlordBefore := e.bal(e.id(e.tenant)), e.bal(e.id(e.landlord))

		e.must(e.call(e.landlord, "ProposeSettlement", strconv.FormatInt(claim, 10), "ipfs://r"))
		var damage int64
		switch path {
		case 0:
			e.must(e.call(e.tenant, "AcceptSettlement"))
			damage = claim
		case 1:
			e.now = e.lease().ChallengeDeadline + 1
			e.must(e.call(e.other, "FinalizeSettlement"))
			damage = claim
		case 2:
			e.must(e.call(e.tenant, "DisputeSettlement", "ipfs://t"))
			e.must(e.call(e.arbitrator, "ResolveDispute", strconv.FormatInt(award, 10)))
			damage = award
		case 3:
			e.must(e.call(e.tenant, "DisputeSettlement", "ipfs://t"))
			e.now = e.lease().ArbitrationDeadline + 1
			e.must(e.call(e.other, "ClaimAfterTimeout"))
		}
		e.withdrawAll()
		want := min(unpaid+damage, int64(deposit))
		l := e.lease()
		if l.PaidToLandlord+l.RefundedToTenant != deposit || l.PaidToLandlord != want ||
			e.bal(e.id(e.landlord))-landlordBefore != want || e.bal(e.id(e.tenant))-tenantBefore != deposit-want ||
			e.bal(escrow) != 0 {
			t.Fatalf("run %d (paid=%d claim=%d award=%d path=%d): conservation violated: %+v", run, paid, claim, award, path, l)
		}
	}
}

// ------------------------------------------------------------------ stateful random sequences
// Port of the Solidity invariant suite: random actions and time jumps, invariants checked after
// every transaction (successful or not).
func TestInvariantsUnderRandomSequences(t *testing.T) {
	rng := rand.New(rand.NewSource(7))
	reached := map[string]int{}
	for run := 0; run < 200; run++ {
		e := newEnv(t)
		e.activate()
		landlordStart := e.bal(e.id(e.landlord))
		maxState := rental.StateActive
		for step := 0; step < 200; step++ {
			var r fabtest.Result
			action := rng.Intn(13)
			switch action {
			case 0:
				e.now += rng.Int63n(35 * 86400)
				continue
			case 1:
				l := e.lease()
				if d := l.LeaseStart + l.PeriodsPaid*period; d > e.now {
					e.now = d
				}
				continue
			case 2:
				e.now = max(e.now, e.lease().LeaseEnd+rng.Int63n(60*86400))
				continue
			case 3:
				r = e.call(e.tenant, "PayRent")
			case 4:
				claim := rng.Int63n(deposit + 1)
				ev := ""
				if rng.Intn(4) > 0 {
					ev = "ipfs://r"
				}
				r = e.call(e.landlord, "ProposeSettlement", strconv.FormatInt(claim, 10), ev)
			case 5:
				r = e.call(e.tenant, "AcceptSettlement")
			case 6:
				r = e.call(e.tenant, "DisputeSettlement", "ipfs://t")
			case 7:
				r = e.call(e.other, "FinalizeSettlement")
			case 8:
				r = e.call(e.arbitrator, "ResolveDispute", strconv.FormatInt(rng.Int63n(deposit+1), 10))
			case 9:
				r = e.call(e.other, "ClaimAfterTimeout")
			case 10:
				if rng.Intn(2) == 0 {
					r = e.call(e.landlord, "Withdraw")
				} else {
					r = e.call(e.tenant, "Withdraw")
				}
			case 12: // punctual tenant: pay up to n periods, each on its due date
				n := 1 + rng.Intn(12)
				for i := 0; i < n; i++ {
					l := e.lease()
					if l.State != rental.StateActive || l.PeriodsPaid >= nPeriods {
						break
					}
					d := l.LeaseStart + l.PeriodsPaid*period
					if d >= l.LeaseEnd {
						break
					}
					e.now = max(e.now, d)
					if r = e.call(e.tenant, "PayRent"); !r.OK() {
						break
					}
				}
			case 11: // adversarial caller must never succeed
				for _, fn := range []string{"PayRent", "AcceptSettlement"} {
					if e.call(e.other, fn).OK() {
						t.Fatalf("stranger succeeded in %s", fn)
					}
				}
				if e.call(e.other, "ProposeSettlement", "1", "x").OK() || e.call(e.other, "ResolveDispute", "0").OK() {
					t.Fatal("stranger succeeded in privileged call")
				}
				continue
			}
			if r.OK() {
				reached[fmt.Sprint(action)]++
			}
			// invariants
			l := e.lease()
			if e.bal(escrow) < l.DepositHeld+l.WithdrawableLandlord+l.WithdrawableTenant {
				t.Fatalf("I1 violated: escrow %d below liabilities %+v", e.bal(escrow), l)
			}
			if l.State == rental.StateSettled {
				if l.DepositHeld != 0 || l.PaidToLandlord+l.RefundedToTenant != deposit {
					t.Fatalf("I2 violated after settlement: %+v", l)
				}
			} else if l.DepositHeld != deposit || l.PaidToLandlord+l.RefundedToTenant != 0 {
				t.Fatalf("I2 violated before settlement: %+v", l)
			}
			received := e.bal(e.id(e.landlord)) - landlordStart + l.WithdrawableLandlord
			if received != l.PeriodsPaid*rent+l.PaidToLandlord ||
				l.PaidToLandlord > (nPeriods-l.PeriodsPaid)*rent+l.DamageClaim {
				t.Fatalf("I3 violated: %+v", l)
			}
			if l.PeriodsPaid > nPeriods {
				t.Fatalf("I4 violated: %+v", l)
			}
			if l.State < maxState {
				t.Fatalf("I5 violated: state went backwards %d -> %d", maxState, l.State)
			}
			maxState = max(maxState, l.State)
			if l.State == rental.StateSettled && l.PeriodsPaid == nPeriods {
				reached["fully-paid-settled"]++
			}
		}
	}
	// Reachability: every settlement route must have been exercised.
	for _, k := range []string{"3", "4", "5", "6", "7", "8", "9", "10", "12", "fully-paid-settled"} {
		if reached[k] == 0 {
			t.Errorf("route %s never reached", k)
		}
	}
	t.Logf("successful actions per route: %v", reached)
}

// guard against accidental use of raw IDs in tests
var _ = base64.StdEncoding
