// SPDX-License-Identifier: MIT
pragma solidity 0.8.24;

import {Test} from "forge-std/Test.sol";
import {BaseTest} from "./Base.t.sol";
import {RentalAgreement} from "../src/RentalAgreement.sol";
import {MockStablecoin} from "./mocks/MockStablecoin.sol";

/// @notice Drives the lease through random sequences of party actions and time jumps.
contract LeaseHandler is Test {
    RentalAgreement internal lease;
    MockStablecoin internal usd;
    address internal landlord;
    address internal tenant;
    address internal arbitrator;

    uint8 public maxStateSeen;
    mapping(bytes32 => uint256) public ok; // successful calls per action
    bool public stateWentBackwards;

    constructor(RentalAgreement _lease, MockStablecoin _usd, address _l, address _t, address _a) {
        lease = _lease;
        usd = _usd;
        landlord = _l;
        tenant = _t;
        arbitrator = _a;
    }

    modifier track() {
        _;
        uint8 s = uint8(lease.state());
        if (s < maxStateSeen) stateWentBackwards = true;
        if (s > maxStateSeen) maxStateSeen = s;
    }

    function warp(uint256 secondsAhead) external track {
        vm.warp(block.timestamp + bound(secondsAhead, 0, 35 days));
    }

    /// @dev Moves to the due date of the next unpaid period (lets the fuzzer complete full leases).
    function toNextDueDate() external track {
        uint64 due = lease.dueDate(lease.periodsPaid());
        if (due > block.timestamp) vm.warp(due);
    }

    /// @dev Jumps to the lease end plus an offset spanning all settlement windows.
    function toLeaseEnd(uint256 offset) external track {
        uint256 target = uint256(lease.leaseEnd()) + bound(offset, 0, 60 days);
        if (target > block.timestamp) vm.warp(target);
    }

    /// @dev Pays up to `n` consecutive periods, each on its due date (models a punctual tenant).
    function payOnSchedule(uint256 n) external track {
        n = bound(n, 1, 12);
        for (uint256 i = 0; i < n; i++) {
            if (lease.state() != RentalAgreement.State.Active || lease.periodsPaid() >= lease.numPeriods()) return;
            uint64 due = lease.dueDate(lease.periodsPaid());
            if (due >= lease.leaseEnd()) return;
            if (due > block.timestamp) vm.warp(due);
            vm.prank(tenant);
            try lease.payRent() {
                ok["payRent"]++;
            } catch {
                return;
            }
        }
    }

    function payRent() external track {
        vm.prank(tenant);
        try lease.payRent() {
            ok["payRent"]++;
        } catch {}
    }

    function propose(uint256 claim, bool withEvidence) external track {
        claim = bound(claim, 0, lease.depositAmount());
        vm.prank(landlord);
        try lease.proposeSettlement(claim, withEvidence ? keccak256("report") : bytes32(0)) {
            ok["propose"]++;
        } catch {}
    }

    function accept() external track {
        vm.prank(tenant);
        try lease.acceptSettlement() {
            ok["accept"]++;
        } catch {}
    }

    function dispute() external track {
        vm.prank(tenant);
        try lease.disputeSettlement(keccak256("tenant-evidence")) {
            ok["dispute"]++;
        } catch {}
    }

    function finalize() external track {
        try lease.finalizeSettlement() {
            ok["finalize"]++;
        } catch {}
    }

    function resolve(uint256 award) external track {
        award = bound(award, 0, lease.depositAmount());
        vm.prank(arbitrator);
        try lease.resolveDispute(award) {
            ok["resolve"]++;
        } catch {}
    }

    function withdraw(bool asLandlord) external track {
        vm.prank(asLandlord ? landlord : tenant);
        try lease.withdraw() {
            ok["withdraw"]++;
        } catch {}
    }

    function timeout() external track {
        try lease.claimAfterTimeout() {
            ok["timeout"]++;
        } catch {}
    }

    /// @dev Adversarial callers: none of these may ever succeed.
    function strangerCalls(uint256 x) external track {
        address stranger = address(uint160(bound(x, 1, type(uint160).max)));
        if (stranger == landlord || stranger == tenant || stranger == arbitrator) return;
        vm.startPrank(stranger);
        try lease.payRent() {
            revert("stranger paid rent");
        } catch {}
        try lease.proposeSettlement(1, keccak256("x")) {
            revert("stranger proposed");
        } catch {}
        try lease.resolveDispute(0) {
            revert("stranger resolved");
        } catch {}
        try lease.acceptSettlement() {
            revert("stranger accepted");
        } catch {}
        vm.stopPrank();
    }
}

contract LeaseInvariantTest is BaseTest {
    LeaseHandler internal handler;
    uint256 internal landlordStart;

    function setUp() public override {
        super.setUp();
        _activate();
        landlordStart = usd.balanceOf(landlord);
        handler = new LeaseHandler(lease, usd, landlord, tenant, arbitrator);
        targetContract(address(handler));
    }

    /// I1: the contract holds at least the escrowed deposit plus allocated, unwithdrawn shares
    /// (solvency). Tokens sent without a protocol call are surplus, not liabilities; see
    /// test_UnsolicitedTransfersDoNotChangeAllocation.
    function invariant_BalanceCoversLiabilities() public view {
        assertGe(
            usd.balanceOf(address(lease)),
            lease.depositHeld() + lease.withdrawable(landlord) + lease.withdrawable(tenant)
        );
    }

    /// I2: before settlement the full deposit is held; after settlement it is fully distributed.
    function invariant_DepositConserved() public view {
        if (lease.state() == RentalAgreement.State.Settled) {
            assertEq(lease.depositHeld(), 0);
            assertEq(lease.paidToLandlord() + lease.refundedToTenant(), DEPOSIT);
        } else {
            assertEq(lease.depositHeld(), DEPOSIT);
            assertEq(lease.paidToLandlord() + lease.refundedToTenant(), 0);
        }
    }

    /// I3: the landlord never receives more than rent paid + unpaid rent + the claimed damage.
    function invariant_LandlordBounded() public view {
        uint256 received = usd.balanceOf(landlord) - landlordStart + lease.withdrawable(landlord);
        assertEq(received, lease.periodsPaid() * RENT + lease.paidToLandlord());
        uint256 unpaid = (uint256(N_PERIODS) - lease.periodsPaid()) * RENT;
        assertLe(lease.paidToLandlord(), unpaid + lease.damageClaim());
    }

    /// I4: rent is never paid for more periods than the lease defines.
    function invariant_NoOverpayment() public view {
        assertLe(lease.periodsPaid(), N_PERIODS);
    }

    /// I5: the lifecycle state never moves backwards.
    function invariant_StateMonotonic() public view {
        assertFalse(handler.stateWentBackwards());
    }
}
