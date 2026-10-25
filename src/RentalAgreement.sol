// SPDX-License-Identifier: MIT
pragma solidity 0.8.24;

import {IERC20} from "@openzeppelin/contracts/token/ERC20/IERC20.sol";
import {SafeERC20} from "@openzeppelin/contracts/token/ERC20/utils/SafeERC20.sol";
import {ReentrancyGuard} from "@openzeppelin/contracts/utils/ReentrancyGuard.sol";
import {IdentityRegistry} from "./IdentityRegistry.sol";

/// @title RentalAgreement
/// @notice Single landlord-tenant lease with (i) identity binding through attested
///         credentials, (ii) a periodic rent schedule with on-chain arrears tracking, and
///         (iii) deposit escrow settled through a propose / challenge / arbitrate protocol.
///
/// Lifecycle (state machine):
///
///   Created --lockDeposit--> Active --proposeSettlement--> SettlementProposed
///                              |                              |  \
///                              |                    accept /  |   \ disputeSettlement
///                              |              finalize (timeout)    \
///                              |                              v      v
///                              +--claimAfterTimeout-------> Settled <-- Disputed
///                                 (no proposal in time)        ^   resolveDispute /
///                                                              +-- claimAfterTimeout
///
/// Design rules:
///  - All terms are fixed at construction (no re-initialisation is possible).
///  - The deposit token is fixed at construction; callers never pass a token address.
///  - Unpaid rent is computed from on-chain records and is deducted without discretion.
///  - A damage deduction requires a landlord proposal bound to an evidence hash, can be
///    challenged by the tenant within a fixed window, and, if challenged, can only be
///    reduced (never increased) by the pre-agreed arbitrator.
///  - Every waiting state has a deadline after which anyone can force progress, so the
///    deposit cannot be frozen by an inactive party. Settlement only allocates shares;
///    each party withdraws separately (pull pattern), so a transfer failure affecting one
///    party (e.g. a stablecoin blocklist) cannot block settlement or the other party. Defaults favour the tenant for the
///    contested (damage) portion and the landlord for the objective (unpaid-rent) portion.
contract RentalAgreement is ReentrancyGuard {
    using SafeERC20 for IERC20;

    enum State {
        Created,
        Active,
        SettlementProposed,
        Disputed,
        Settled
    }

    struct Terms {
        address landlord;
        address tenant;
        address arbitrator;
        IERC20 token;
        bytes32 propertyId;
        uint256 rentAmount;
        uint256 depositAmount;
        uint32 periodLength; // seconds per rent period (e.g. 30 days)
        uint16 numPeriods; // number of rent periods in the lease
        uint32 proposalWindow; // time after lease end for the landlord to propose a settlement
        uint32 challengeWindow; // time for the tenant to accept or dispute a proposal
        uint32 arbitrationWindow; // time for the arbitrator to resolve a dispute
    }

    // ---------------------------------------------------------------- immutable terms
    IdentityRegistry public immutable registry;
    address public immutable landlord;
    address public immutable tenant;
    address public immutable arbitrator;
    IERC20 public immutable token;
    bytes32 public immutable propertyId;
    uint256 public immutable rentAmount;
    uint256 public immutable depositAmount;
    uint32 public immutable periodLength;
    uint16 public immutable numPeriods;
    uint32 public immutable proposalWindow;
    uint32 public immutable challengeWindow;
    uint32 public immutable arbitrationWindow;

    // ---------------------------------------------------------------- mutable state
    State public state;
    bytes32 public landlordDid;
    bytes32 public tenantDid;

    uint64 public leaseStart;
    uint64 public leaseEnd;
    uint16 public periodsPaid;
    mapping(uint256 => uint64) public paidAt; // period index => payment timestamp

    uint256 public depositHeld;
    uint256 public damageClaim;
    bytes32 public landlordEvidence;
    bytes32 public tenantEvidence;
    uint64 public challengeDeadline;
    uint64 public arbitrationDeadline;

    uint256 public paidToLandlord; // deposit share allocated to landlord at settlement
    uint256 public refundedToTenant; // deposit share allocated to tenant at settlement
    mapping(address => uint256) public withdrawable; // allocated but not yet withdrawn

    // ---------------------------------------------------------------- events
    event IdentityBound(address indexed party, uint8 role, bytes32 didHash, address indexed issuer);
    event DepositLocked(uint256 amount, uint64 leaseStart, uint64 leaseEnd);
    event RentPaid(uint256 indexed period, uint256 amount, uint64 dueAt, uint64 paidAt);
    event SettlementProposed(uint256 damageClaim, bytes32 evidence, uint64 challengeDeadline);
    event SettlementDisputed(bytes32 evidence, uint64 arbitrationDeadline);
    event DisputeResolved(uint256 awardedDamage);
    event DepositSettled(uint256 toLandlord, uint256 toTenant, uint256 unpaidRentPortion, uint256 damagePortion);
    event Withdrawn(address indexed party, uint256 amount);

    // ---------------------------------------------------------------- errors
    error InvalidTerms();
    error Unauthorized();
    error WrongState(State expected, State actual);
    error IdentityMismatch();
    error IdentityNotBound();
    error TooEarly();
    error TooLate();
    error LeaseFullyPaid();
    error ClaimExceedsDeposit();
    error MissingEvidence();
    error AwardExceedsClaim();
    error UnsupportedToken();
    error NothingToWithdraw();

    modifier onlyLandlord() {
        if (msg.sender != landlord) revert Unauthorized();
        _;
    }

    modifier onlyTenant() {
        if (msg.sender != tenant) revert Unauthorized();
        _;
    }

    modifier inState(State s) {
        if (state != s) revert WrongState(s, state);
        _;
    }

    constructor(Terms memory t, IdentityRegistry _registry) {
        if (
            address(_registry) == address(0) || t.landlord == address(0) || t.tenant == address(0)
                || t.arbitrator == address(0) || address(t.token) == address(0) || t.landlord == t.tenant
                || t.arbitrator == t.landlord || t.arbitrator == t.tenant || t.propertyId == bytes32(0)
                || t.rentAmount == 0 || t.depositAmount == 0 || t.periodLength == 0 || t.numPeriods == 0
                || t.proposalWindow == 0 || t.challengeWindow == 0 || t.arbitrationWindow == 0
        ) revert InvalidTerms();

        registry = _registry;
        landlord = t.landlord;
        tenant = t.tenant;
        arbitrator = t.arbitrator;
        token = t.token;
        propertyId = t.propertyId;
        rentAmount = t.rentAmount;
        depositAmount = t.depositAmount;
        periodLength = t.periodLength;
        numPeriods = t.numPeriods;
        proposalWindow = t.proposalWindow;
        challengeWindow = t.challengeWindow;
        arbitrationWindow = t.arbitrationWindow;
    }

    // ================================================================ phase 1-2: identity binding
    /// @notice Binds the caller's address to an attested identity. Binding also constitutes
    ///         the party's on-chain acceptance of the immutable lease terms.
    function bindIdentity(IdentityRegistry.Attestation calldata a, bytes calldata signature)
        external
        inState(State.Created)
    {
        if (a.subject != msg.sender || a.didHash == bytes32(0)) revert IdentityMismatch();

        if (msg.sender == landlord) {
            if (a.role != registry.ROLE_LANDLORD() || a.propertyId != propertyId) revert IdentityMismatch();
            address issuer = registry.verify(a, signature);
            landlordDid = a.didHash;
            emit IdentityBound(msg.sender, a.role, a.didHash, issuer);
        } else if (msg.sender == tenant) {
            if (a.role != registry.ROLE_TENANT()) revert IdentityMismatch();
            address issuer = registry.verify(a, signature);
            tenantDid = a.didHash;
            emit IdentityBound(msg.sender, a.role, a.didHash, issuer);
        } else {
            revert Unauthorized();
        }
    }

    // ================================================================ phase 3: deposit locking
    function lockDeposit() external onlyTenant inState(State.Created) nonReentrant {
        if (landlordDid == bytes32(0) || tenantDid == bytes32(0)) revert IdentityNotBound();

        // effects
        state = State.Active;
        leaseStart = uint64(block.timestamp);
        leaseEnd = uint64(block.timestamp + uint256(periodLength) * numPeriods);
        depositHeld = depositAmount;

        // interaction (balance check rejects fee-on-transfer / non-standard tokens)
        uint256 before = token.balanceOf(address(this));
        token.safeTransferFrom(msg.sender, address(this), depositAmount); // msg.sender == tenant (onlyTenant)
        if (token.balanceOf(address(this)) - before != depositAmount) revert UnsupportedToken();

        emit DepositLocked(depositAmount, leaseStart, leaseEnd);
    }

    // ================================================================ phase 4: rent execution
    /// @notice Pays the next unpaid rent period. A period can be paid at most one period
    ///         ahead of its due date; payments after the lease end are not accepted (arrears
    ///         are then recovered from the deposit at settlement).
    function payRent() external onlyTenant inState(State.Active) nonReentrant {
        if (block.timestamp >= leaseEnd) revert TooLate();
        uint16 period = periodsPaid;
        if (period >= numPeriods) revert LeaseFullyPaid();
        uint64 due = dueDate(period);
        if (block.timestamp + periodLength < due) revert TooEarly();

        // effects
        periodsPaid = period + 1;
        paidAt[period] = uint64(block.timestamp);

        // interaction: rent flows directly tenant -> landlord, never through the contract
        token.safeTransferFrom(msg.sender, landlord, rentAmount); // msg.sender == tenant (onlyTenant)

        emit RentPaid(period, rentAmount, due, uint64(block.timestamp));
    }

    // ================================================================ phase 5: settlement
    /// @notice Landlord proposes a damage deduction bound to an evidence hash (e.g. the
    ///         IPFS CID digest of an inspection report). Unpaid rent is added automatically.
    ///         A zero claim settles immediately, as nothing remains to be contested.
    function proposeSettlement(uint256 claim, bytes32 evidence) external onlyLandlord inState(State.Active) nonReentrant {
        if (block.timestamp < leaseEnd) revert TooEarly();
        if (block.timestamp > uint256(leaseEnd) + proposalWindow) revert TooLate();
        if (claim > depositAmount) revert ClaimExceedsDeposit();

        if (claim == 0) {
            _settle(0);
            return;
        }
        if (evidence == bytes32(0)) revert MissingEvidence();

        damageClaim = claim;
        landlordEvidence = evidence;
        challengeDeadline = uint64(block.timestamp + challengeWindow);
        state = State.SettlementProposed;
        emit SettlementProposed(claim, evidence, challengeDeadline);
    }

    function acceptSettlement() external onlyTenant inState(State.SettlementProposed) nonReentrant {
        _settle(damageClaim);
    }

    function disputeSettlement(bytes32 evidence) external onlyTenant inState(State.SettlementProposed) {
        if (block.timestamp > challengeDeadline) revert TooLate();
        if (evidence == bytes32(0)) revert MissingEvidence();
        tenantEvidence = evidence;
        arbitrationDeadline = uint64(block.timestamp + arbitrationWindow);
        state = State.Disputed;
        emit SettlementDisputed(evidence, arbitrationDeadline);
    }

    /// @notice Executes an unchallenged proposal once the challenge window has elapsed.
    function finalizeSettlement() external inState(State.SettlementProposed) nonReentrant {
        if (block.timestamp <= challengeDeadline) revert TooEarly();
        _settle(damageClaim);
    }

    /// @notice The arbitrator can confirm or reduce the damage claim, never increase it.
    function resolveDispute(uint256 awardedDamage) external inState(State.Disputed) nonReentrant {
        if (msg.sender != arbitrator) revert Unauthorized();
        if (block.timestamp > arbitrationDeadline) revert TooLate();
        if (awardedDamage > damageClaim) revert AwardExceedsClaim();
        emit DisputeResolved(awardedDamage);
        _settle(awardedDamage);
    }

    /// @notice Liveness guarantee. Callable by anyone when:
    ///   (a) the lease ended and the landlord did not propose within the proposal window, or
    ///   (b) a dispute was raised and the arbitrator did not rule within the arbitration window.
    /// In both cases only the objective unpaid-rent portion is deducted.
    function claimAfterTimeout() external nonReentrant {
        if (state == State.Active) {
            if (block.timestamp <= uint256(leaseEnd) + proposalWindow) revert TooEarly();
        } else if (state == State.Disputed) {
            if (block.timestamp <= arbitrationDeadline) revert TooEarly();
        } else {
            revert WrongState(State.Active, state);
        }
        _settle(0);
    }

    /// @notice Withdraws the caller's settled share of the deposit.
    function withdraw() external nonReentrant {
        uint256 amount = withdrawable[msg.sender];
        if (amount == 0) revert NothingToWithdraw();
        withdrawable[msg.sender] = 0;
        token.safeTransfer(msg.sender, amount);
        emit Withdrawn(msg.sender, amount);
    }

    // ================================================================ views
    function dueDate(uint256 period) public view returns (uint64) {
        return uint64(uint256(leaseStart) + period * periodLength);
    }

    /// @notice Number of periods whose due date has been reached.
    function periodsDue() public view returns (uint256) {
        if (state == State.Created) return 0;
        if (block.timestamp >= leaseEnd) return numPeriods;
        return (block.timestamp - leaseStart) / periodLength + 1;
    }

    function arrears() public view returns (uint256) {
        uint256 due = periodsDue();
        return due > periodsPaid ? due - periodsPaid : 0;
    }

    // ================================================================ internal
    function _settle(uint256 damage) internal {
        uint256 held = depositHeld;
        uint256 unpaid = arrears() * rentAmount;

        uint256 unpaidPortion = unpaid < held ? unpaid : held;
        uint256 remaining = held - unpaidPortion;
        uint256 damagePortion = damage < remaining ? damage : remaining;
        uint256 toLandlord = unpaidPortion + damagePortion;
        uint256 toTenant = held - toLandlord;

        // effects only: shares are allocated here and withdrawn separately
        state = State.Settled;
        depositHeld = 0;
        paidToLandlord = toLandlord;
        refundedToTenant = toTenant;
        withdrawable[landlord] += toLandlord;
        withdrawable[tenant] += toTenant;

        emit DepositSettled(toLandlord, toTenant, unpaidPortion, damagePortion);
    }
}
