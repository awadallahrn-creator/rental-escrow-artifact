// SPDX-License-Identifier: MIT
pragma solidity 0.8.24;

import {IERC20} from "@openzeppelin/contracts/token/ERC20/IERC20.sol";
import {BaseTest} from "./Base.t.sol";
import {IdentityRegistry} from "../src/IdentityRegistry.sol";
import {RentalAgreement} from "../src/RentalAgreement.sol";
import {FeeOnTransferToken} from "./mocks/MockStablecoin.sol";

contract IdentityBindingTest is BaseTest {
    function test_BindBothParties() public {
        _bindLandlord();
        _bindTenant();
        assertEq(lease.landlordDid(), LANDLORD_DID);
        assertEq(lease.tenantDid(), TENANT_DID);
    }

    function test_RevertWhen_LockWithoutIdentity() public {
        _bindLandlord();
        vm.prank(tenant);
        vm.expectRevert(RentalAgreement.IdentityNotBound.selector);
        lease.lockDeposit();
    }

    function test_RevertWhen_UntrustedIssuer() public {
        IdentityRegistry.Attestation memory a = _attestation(tenant, TENANT_DID, 2, bytes32(0));
        bytes memory sig = _sign(ROGUE_PK, a);
        vm.prank(tenant);
        vm.expectRevert(abi.encodeWithSelector(IdentityRegistry.UntrustedIssuer.selector, vm.addr(ROGUE_PK)));
        lease.bindIdentity(a, sig);
    }

    function test_RevertWhen_IssuerRemoved() public {
        vm.prank(governor);
        registry.removeIssuer(issuer);
        IdentityRegistry.Attestation memory a = _attestation(tenant, TENANT_DID, 2, bytes32(0));
        bytes memory sig = _sign(ISSUER_PK, a);
        vm.prank(tenant);
        vm.expectRevert(abi.encodeWithSelector(IdentityRegistry.UntrustedIssuer.selector, issuer));
        lease.bindIdentity(a, sig);
    }

    function test_RevertWhen_AttestationExpired() public {
        IdentityRegistry.Attestation memory a = _attestation(tenant, TENANT_DID, 2, bytes32(0));
        bytes memory sig = _sign(ISSUER_PK, a);
        vm.warp(a.expiresAt + 1);
        vm.prank(tenant);
        vm.expectRevert(IdentityRegistry.AttestationExpired.selector);
        lease.bindIdentity(a, sig);
    }

    function test_RevertWhen_AttestationRevoked() public {
        IdentityRegistry.Attestation memory a = _attestation(tenant, TENANT_DID, 2, bytes32(0));
        bytes memory sig = _sign(ISSUER_PK, a);
        vm.prank(issuer);
        registry.revoke(a, sig);
        vm.prank(tenant);
        vm.expectRevert(IdentityRegistry.AttestationIsRevoked.selector);
        lease.bindIdentity(a, sig);
    }

    function test_RevertWhen_NonIssuerRevokes() public {
        IdentityRegistry.Attestation memory a = _attestation(tenant, TENANT_DID, 2, bytes32(0));
        bytes memory sig = _sign(ISSUER_PK, a);
        vm.prank(tenant);
        vm.expectRevert(IdentityRegistry.NotIssuer.selector);
        registry.revoke(a, sig);
    }

    function test_RevertWhen_AttestationForAnotherAddress() public {
        // tenant tries to use the landlord's attestation
        IdentityRegistry.Attestation memory a = _attestation(landlord, LANDLORD_DID, 1, PROPERTY);
        bytes memory sig = _sign(ISSUER_PK, a);
        vm.prank(tenant);
        vm.expectRevert(RentalAgreement.IdentityMismatch.selector);
        lease.bindIdentity(a, sig);
    }

    function test_RevertWhen_WrongRole() public {
        IdentityRegistry.Attestation memory a = _attestation(landlord, LANDLORD_DID, 2, PROPERTY);
        bytes memory sig = _sign(ISSUER_PK, a);
        vm.prank(landlord);
        vm.expectRevert(RentalAgreement.IdentityMismatch.selector);
        lease.bindIdentity(a, sig);
    }

    function test_RevertWhen_LandlordAttestedForOtherProperty() public {
        IdentityRegistry.Attestation memory a = _attestation(landlord, LANDLORD_DID, 1, keccak256("other-unit"));
        bytes memory sig = _sign(ISSUER_PK, a);
        vm.prank(landlord);
        vm.expectRevert(RentalAgreement.IdentityMismatch.selector);
        lease.bindIdentity(a, sig);
    }

    function test_RevertWhen_TamperedAttestation() public {
        IdentityRegistry.Attestation memory a = _attestation(tenant, TENANT_DID, 2, bytes32(0));
        bytes memory sig = _sign(ISSUER_PK, a);
        a.didHash = keccak256("did:example:someone-else");
        vm.prank(tenant);
        vm.expectRevert(); // recovers a different (untrusted) signer
        lease.bindIdentity(a, sig);
    }

    function test_RevertWhen_ThirdPartyBinds() public {
        address stranger = makeAddr("stranger");
        IdentityRegistry.Attestation memory a = _attestation(stranger, TENANT_DID, 2, bytes32(0));
        bytes memory sig = _sign(ISSUER_PK, a);
        vm.prank(stranger);
        vm.expectRevert(RentalAgreement.Unauthorized.selector);
        lease.bindIdentity(a, sig);
    }

    function test_RevertWhen_BindAfterActivation() public {
        _activate();
        IdentityRegistry.Attestation memory a = _attestation(tenant, TENANT_DID, 2, bytes32(0));
        bytes memory sig = _sign(ISSUER_PK, a);
        vm.prank(tenant);
        vm.expectRevert(
            abi.encodeWithSelector(
                RentalAgreement.WrongState.selector, RentalAgreement.State.Created, RentalAgreement.State.Active
            )
        );
        lease.bindIdentity(a, sig);
    }
}

contract DepositAndRentTest is BaseTest {
    function test_LockDeposit() public {
        _activate();
        assertEq(uint8(lease.state()), uint8(RentalAgreement.State.Active));
        assertEq(usd.balanceOf(address(lease)), DEPOSIT);
        assertEq(lease.depositHeld(), DEPOSIT);
        assertEq(lease.leaseEnd(), lease.leaseStart() + uint64(PERIOD) * N_PERIODS);
    }

    function test_RevertWhen_NonTenantLocks() public {
        _bindLandlord();
        _bindTenant();
        vm.prank(landlord);
        vm.expectRevert(RentalAgreement.Unauthorized.selector);
        lease.lockDeposit();
    }

    function test_RevertWhen_LockTwice() public {
        _activate();
        vm.prank(tenant);
        vm.expectRevert();
        lease.lockDeposit();
    }

    function test_RentGoesDirectlyToLandlord() public {
        _activate();
        _payPeriods(3);
        assertEq(usd.balanceOf(landlord), 3 * RENT);
        assertEq(usd.balanceOf(address(lease)), DEPOSIT); // contract never holds rent
        assertEq(lease.periodsPaid(), 3);
        assertEq(lease.paidAt(2), lease.dueDate(2));
    }

    function test_RevertWhen_PayingTwoPeriodsAhead() public {
        _activate();
        vm.startPrank(tenant);
        lease.payRent(); // period 0 (due now)
        lease.payRent(); // period 1 (one period ahead: allowed)
        vm.expectRevert(RentalAgreement.TooEarly.selector);
        lease.payRent(); // period 2: too early
        vm.stopPrank();
    }

    function test_RevertWhen_PayAfterLeaseEnd() public {
        _activate();
        _toLeaseEnd();
        vm.prank(tenant);
        vm.expectRevert(RentalAgreement.TooLate.selector);
        lease.payRent();
    }

    function test_RevertWhen_LeaseFullyPaid() public {
        _activate();
        _payPeriods(N_PERIODS);
        vm.prank(tenant);
        vm.expectRevert(RentalAgreement.LeaseFullyPaid.selector);
        lease.payRent();
    }

    function test_ArrearsTracking() public {
        _activate();
        _payPeriods(2);
        vm.warp(lease.dueDate(4)); // periods 0..4 are due, 2 paid
        assertEq(lease.periodsDue(), 5);
        assertEq(lease.arrears(), 3);
    }

    function test_RevertWhen_NonStandardToken() public {
        FeeOnTransferToken fee = new FeeOnTransferToken();
        RentalAgreement.Terms memory t = _terms();
        t.token = IERC20(address(fee));
        lease = new RentalAgreement(t, registry);
        fee.mint(tenant, 10_000e6);
        vm.prank(tenant);
        fee.approve(address(lease), type(uint256).max);
        _bindLandlord();
        _bindTenant();
        vm.prank(tenant);
        vm.expectRevert(RentalAgreement.UnsupportedToken.selector);
        lease.lockDeposit();
    }

    function test_RevertWhen_InvalidTerms() public {
        RentalAgreement.Terms memory t = _terms();
        t.arbitrator = landlord;
        vm.expectRevert(RentalAgreement.InvalidTerms.selector);
        new RentalAgreement(t, registry);

        t = _terms();
        t.depositAmount = 0;
        vm.expectRevert(RentalAgreement.InvalidTerms.selector);
        new RentalAgreement(t, registry);
    }

    /// I1 is a solvency bound (balance >= liabilities): tokens sent to the contract without a
    /// protocol call are not liabilities. They do not change the allocation or block withdrawals,
    /// and no function can move them (they remain in the contract).
    function test_UnsolicitedTransfersDoNotChangeAllocation() public {
        address donor = makeAddr("donor");
        usd.mint(donor, 30e6);

        // before the deposit is locked (state Created)
        vm.prank(donor);
        usd.transfer(address(lease), 10e6);
        _activate(); // the lock checks the balance delta, so the earlier transfer does not interfere
        assertEq(lease.depositHeld(), DEPOSIT);

        // during the lease (state Active)
        vm.prank(donor);
        usd.transfer(address(lease), 10e6);
        _payPeriods(N_PERIODS);
        _toLeaseEnd();
        vm.prank(landlord);
        lease.proposeSettlement(300e6, keccak256("ipfs://inspection-report"));
        vm.prank(tenant);
        lease.acceptSettlement();
        assertEq(lease.paidToLandlord(), 300e6); // same allocation as test_TenantAcceptsClaim
        assertEq(lease.refundedToTenant(), 700e6);

        // after settlement (state Settled)
        vm.prank(donor);
        usd.transfer(address(lease), 10e6);
        assertEq(usd.balanceOf(address(lease)), DEPOSIT + 30e6);

        uint256 l0 = usd.balanceOf(landlord);
        uint256 t0 = usd.balanceOf(tenant);
        vm.prank(landlord);
        lease.withdraw();
        vm.prank(tenant);
        lease.withdraw();
        assertEq(usd.balanceOf(landlord) - l0, 300e6); // each party receives exactly its share
        assertEq(usd.balanceOf(tenant) - t0, 700e6);
        assertEq(lease.depositHeld() + lease.withdrawable(landlord) + lease.withdrawable(tenant), 0);
        assertEq(usd.balanceOf(address(lease)), 30e6); // surplus is neither allocated nor withdrawable
    }
}

contract SettlementTest is BaseTest {
    bytes32 internal constant REPORT = keccak256("ipfs://inspection-report");
    bytes32 internal constant TENANT_REPORT = keccak256("ipfs://tenant-photos");

    function setUp() public override {
        super.setUp();
        _activate();
    }

    function _endFullyPaid() internal {
        _payPeriods(N_PERIODS);
        _toLeaseEnd();
    }

    function test_ZeroClaimRefundsFullDeposit() public {
        _endFullyPaid();
        uint256 before = usd.balanceOf(tenant);
        vm.prank(landlord);
        lease.proposeSettlement(0, bytes32(0));
        assertEq(uint8(lease.state()), uint8(RentalAgreement.State.Settled));
        assertEq(usd.balanceOf(address(lease)), DEPOSIT); // allocated, not yet withdrawn
        vm.prank(tenant);
        lease.withdraw();
        assertEq(usd.balanceOf(tenant) - before, DEPOSIT);
        assertEq(usd.balanceOf(address(lease)), 0);
    }

    function test_RevertWhen_ProposeBeforeLeaseEnd() public {
        vm.prank(landlord);
        vm.expectRevert(RentalAgreement.TooEarly.selector);
        lease.proposeSettlement(100e6, REPORT);
    }

    function test_RevertWhen_ClaimWithoutEvidence() public {
        _endFullyPaid();
        vm.prank(landlord);
        vm.expectRevert(RentalAgreement.MissingEvidence.selector);
        lease.proposeSettlement(100e6, bytes32(0));
    }

    function test_RevertWhen_ClaimExceedsDeposit() public {
        _endFullyPaid();
        vm.prank(landlord);
        vm.expectRevert(RentalAgreement.ClaimExceedsDeposit.selector);
        lease.proposeSettlement(DEPOSIT + 1, REPORT);
    }

    function test_RevertWhen_TenantProposes() public {
        _endFullyPaid();
        vm.prank(tenant);
        vm.expectRevert(RentalAgreement.Unauthorized.selector);
        lease.proposeSettlement(0, bytes32(0));
    }

    function test_TenantAcceptsClaim() public {
        _endFullyPaid();
        vm.prank(landlord);
        lease.proposeSettlement(300e6, REPORT);
        vm.prank(tenant);
        lease.acceptSettlement();
        assertEq(lease.paidToLandlord(), 300e6);
        assertEq(lease.refundedToTenant(), 700e6);
    }

    function test_UnchallengedClaimFinalizesAfterWindow() public {
        _endFullyPaid();
        vm.prank(landlord);
        lease.proposeSettlement(300e6, REPORT);

        vm.expectRevert(RentalAgreement.TooEarly.selector);
        lease.finalizeSettlement();

        vm.warp(lease.challengeDeadline() + 1);
        vm.prank(makeAddr("anyone"));
        lease.finalizeSettlement();
        assertEq(lease.paidToLandlord(), 300e6);
        assertEq(lease.refundedToTenant(), 700e6);
    }

    function test_RevertWhen_DisputeAfterWindow() public {
        _endFullyPaid();
        vm.prank(landlord);
        lease.proposeSettlement(300e6, REPORT);
        vm.warp(lease.challengeDeadline() + 1);
        vm.prank(tenant);
        vm.expectRevert(RentalAgreement.TooLate.selector);
        lease.disputeSettlement(TENANT_REPORT);
    }

    function test_ArbitratorReducesClaim() public {
        _endFullyPaid();
        vm.prank(landlord);
        lease.proposeSettlement(600e6, REPORT);
        vm.prank(tenant);
        lease.disputeSettlement(TENANT_REPORT);

        vm.prank(arbitrator);
        lease.resolveDispute(150e6);
        assertEq(lease.paidToLandlord(), 150e6);
        assertEq(lease.refundedToTenant(), 850e6);
    }

    function test_RevertWhen_ArbitratorExceedsClaim() public {
        _endFullyPaid();
        vm.prank(landlord);
        lease.proposeSettlement(200e6, REPORT);
        vm.prank(tenant);
        lease.disputeSettlement(TENANT_REPORT);
        vm.prank(arbitrator);
        vm.expectRevert(RentalAgreement.AwardExceedsClaim.selector);
        lease.resolveDispute(200e6 + 1);
    }

    function test_RevertWhen_LandlordResolvesOwnDispute() public {
        _endFullyPaid();
        vm.prank(landlord);
        lease.proposeSettlement(200e6, REPORT);
        vm.prank(tenant);
        lease.disputeSettlement(TENANT_REPORT);
        vm.prank(landlord);
        vm.expectRevert(RentalAgreement.Unauthorized.selector);
        lease.resolveDispute(200e6);
    }

    function test_InactiveArbitratorDefaultsToTenantOnDamage() public {
        _endFullyPaid();
        vm.prank(landlord);
        lease.proposeSettlement(600e6, REPORT);
        vm.prank(tenant);
        lease.disputeSettlement(TENANT_REPORT);

        vm.expectRevert(RentalAgreement.TooEarly.selector);
        lease.claimAfterTimeout();

        vm.warp(lease.arbitrationDeadline() + 1);
        lease.claimAfterTimeout();
        assertEq(lease.paidToLandlord(), 0);
        assertEq(lease.refundedToTenant(), DEPOSIT);
    }

    function test_InactiveLandlordCannotFreezeDeposit() public {
        _endFullyPaid();
        vm.warp(uint256(lease.leaseEnd()) + PROPOSAL_WINDOW + 1);
        vm.prank(tenant);
        lease.claimAfterTimeout();
        assertEq(lease.refundedToTenant(), DEPOSIT);

        // the landlord can no longer propose
        vm.prank(landlord);
        vm.expectRevert();
        lease.proposeSettlement(100e6, REPORT);
    }

    function test_UnpaidRentDeductedWithoutDiscretion() public {
        _payPeriods(10); // 2 periods unpaid
        _toLeaseEnd();
        vm.prank(landlord);
        lease.proposeSettlement(0, bytes32(0));
        assertEq(lease.paidToLandlord(), 2 * RENT);
        assertEq(lease.refundedToTenant(), DEPOSIT - 2 * RENT);
    }

    function test_UnpaidRentPlusDamageCappedAtDeposit() public {
        _payPeriods(11); // 1 period unpaid = 500
        _toLeaseEnd();
        vm.prank(landlord);
        lease.proposeSettlement(800e6, REPORT); // 500 + 800 > 1000
        vm.prank(tenant);
        lease.acceptSettlement();
        assertEq(lease.paidToLandlord(), DEPOSIT);
        assertEq(lease.refundedToTenant(), 0);
    }

    function test_ArrearsStillDeductedWhenArbitratorInactive() public {
        _payPeriods(11);
        _toLeaseEnd();
        vm.prank(landlord);
        lease.proposeSettlement(300e6, REPORT);
        vm.prank(tenant);
        lease.disputeSettlement(TENANT_REPORT);
        vm.warp(lease.arbitrationDeadline() + 1);
        lease.claimAfterTimeout();
        assertEq(lease.paidToLandlord(), RENT); // objective portion only
        assertEq(lease.refundedToTenant(), DEPOSIT - RENT);
    }

    function test_RevertWhen_SettleTwice() public {
        _endFullyPaid();
        vm.prank(landlord);
        lease.proposeSettlement(0, bytes32(0));
        vm.expectRevert();
        lease.claimAfterTimeout();
        vm.prank(tenant);
        vm.expectRevert();
        lease.acceptSettlement();
    }

    function test_RevertWhen_WithdrawNothing() public {
        vm.prank(tenant);
        vm.expectRevert(RentalAgreement.NothingToWithdraw.selector);
        lease.withdraw();
    }

    function test_RevertWhen_WithdrawTwice() public {
        _endFullyPaid();
        vm.prank(landlord);
        lease.proposeSettlement(0, bytes32(0));
        vm.startPrank(tenant);
        lease.withdraw();
        vm.expectRevert(RentalAgreement.NothingToWithdraw.selector);
        lease.withdraw();
        vm.stopPrank();
    }

    /// A blocked recipient (e.g. stablecoin blocklist) must not freeze settlement or the other party.
    function test_BlockedTenantCannotFreezeLandlordShare() public {
        _payPeriods(11); // 1 period unpaid -> 500 to landlord, 500 to tenant
        _toLeaseEnd();
        usd.setBlocked(tenant, true); // e.g. issuer blocklist hits the tenant
        vm.prank(landlord);
        lease.proposeSettlement(0, bytes32(0)); // settlement itself must still succeed
        assertEq(uint8(lease.state()), uint8(RentalAgreement.State.Settled));

        uint256 before = usd.balanceOf(landlord);
        vm.prank(landlord);
        lease.withdraw(); // landlord is unaffected
        assertEq(usd.balanceOf(landlord) - before, RENT);

        vm.prank(tenant);
        vm.expectRevert(); // tenant transfer blocked by the token, share stays claimable
        lease.withdraw();
        assertEq(lease.withdrawable(tenant), DEPOSIT - RENT);

        usd.setBlocked(tenant, false); // once unblocked, the tenant can still withdraw
        vm.prank(tenant);
        lease.withdraw();
        assertEq(lease.withdrawable(tenant), 0);
    }

    // -------------------------------------------------------- fuzz: conservation of the deposit
    function testFuzz_SettlementConservesDeposit(uint8 paid, uint256 claim, uint256 award, uint8 path) public {
        paid = uint8(bound(paid, 0, N_PERIODS));
        claim = bound(claim, 1, DEPOSIT);
        award = bound(award, 0, claim);
        path = uint8(bound(path, 0, 3));

        _payPeriods(paid);
        _toLeaseEnd();
        uint256 unpaid = (N_PERIODS - paid) * RENT;
        uint256 tenantBefore = usd.balanceOf(tenant);
        uint256 landlordBefore = usd.balanceOf(landlord);

        vm.prank(landlord);
        lease.proposeSettlement(claim, REPORT);

        uint256 effectiveDamage;
        if (path == 0) {
            vm.prank(tenant);
            lease.acceptSettlement();
            effectiveDamage = claim;
        } else if (path == 1) {
            vm.warp(lease.challengeDeadline() + 1);
            lease.finalizeSettlement();
            effectiveDamage = claim;
        } else if (path == 2) {
            vm.prank(tenant);
            lease.disputeSettlement(TENANT_REPORT);
            vm.prank(arbitrator);
            lease.resolveDispute(award);
            effectiveDamage = award;
        } else {
            vm.prank(tenant);
            lease.disputeSettlement(TENANT_REPORT);
            vm.warp(lease.arbitrationDeadline() + 1);
            lease.claimAfterTimeout();
            effectiveDamage = 0;
        }

        uint256 expectedLandlord = unpaid + effectiveDamage > DEPOSIT ? DEPOSIT : unpaid + effectiveDamage;
        if (lease.withdrawable(landlord) > 0) {
            vm.prank(landlord);
            lease.withdraw();
        }
        if (lease.withdrawable(tenant) > 0) {
            vm.prank(tenant);
            lease.withdraw();
        }
        assertEq(lease.paidToLandlord() + lease.refundedToTenant(), DEPOSIT, "deposit not conserved");
        assertEq(lease.paidToLandlord(), expectedLandlord, "wrong landlord share");
        assertEq(usd.balanceOf(landlord) - landlordBefore, expectedLandlord);
        assertEq(usd.balanceOf(tenant) - tenantBefore, DEPOSIT - expectedLandlord);
        assertEq(usd.balanceOf(address(lease)), 0, "funds left in contract");
    }
}
