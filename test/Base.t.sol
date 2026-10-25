// SPDX-License-Identifier: MIT
pragma solidity 0.8.24;

import {Test} from "forge-std/Test.sol";
import {IERC20} from "@openzeppelin/contracts/token/ERC20/IERC20.sol";
import {IdentityRegistry} from "../src/IdentityRegistry.sol";
import {RentalAgreement} from "../src/RentalAgreement.sol";
import {MockStablecoin} from "./mocks/MockStablecoin.sol";

/// @notice Shared fixture: registry with one trusted issuer, a 12-month lease in a 6-decimal stablecoin.
abstract contract BaseTest is Test {
    uint256 internal constant ISSUER_PK = 0xA11CE;
    uint256 internal constant ROGUE_PK = 0xBAD;

    address internal governor = makeAddr("governor");
    address internal landlord = makeAddr("landlord");
    address internal tenant = makeAddr("tenant");
    address internal arbitrator = makeAddr("arbitrator");
    address internal issuer;

    bytes32 internal constant PROPERTY = keccak256("did:example:property:ramallah-unit-12");
    bytes32 internal constant LANDLORD_DID = keccak256("did:example:landlord-001");
    bytes32 internal constant TENANT_DID = keccak256("did:example:tenant-001");

    uint256 internal constant RENT = 500e6; // 500 mUSD
    uint256 internal constant DEPOSIT = 1_000e6; // 1,000 mUSD
    uint32 internal constant PERIOD = 30 days;
    uint16 internal constant N_PERIODS = 12;
    uint32 internal constant PROPOSAL_WINDOW = 14 days;
    uint32 internal constant CHALLENGE_WINDOW = 14 days;
    uint32 internal constant ARBITRATION_WINDOW = 30 days;

    IdentityRegistry internal registry;
    MockStablecoin internal usd;
    RentalAgreement internal lease;

    function setUp() public virtual {
        issuer = vm.addr(ISSUER_PK);
        registry = new IdentityRegistry(governor);
        vm.prank(governor);
        registry.addIssuer(issuer);

        usd = new MockStablecoin();
        lease = new RentalAgreement(_terms(), registry);

        usd.mint(tenant, 100_000e6);
        vm.prank(tenant);
        usd.approve(address(lease), type(uint256).max);
    }

    function _terms() internal view returns (RentalAgreement.Terms memory t) {
        t = RentalAgreement.Terms({
            landlord: landlord,
            tenant: tenant,
            arbitrator: arbitrator,
            token: IERC20(address(usd)),
            propertyId: PROPERTY,
            rentAmount: RENT,
            depositAmount: DEPOSIT,
            periodLength: PERIOD,
            numPeriods: N_PERIODS,
            proposalWindow: PROPOSAL_WINDOW,
            challengeWindow: CHALLENGE_WINDOW,
            arbitrationWindow: ARBITRATION_WINDOW
        });
    }

    function _attestation(address subject, bytes32 did, uint8 role, bytes32 property)
        internal
        view
        returns (IdentityRegistry.Attestation memory a)
    {
        a = IdentityRegistry.Attestation({
            subject: subject,
            didHash: did,
            role: role,
            propertyId: property,
            expiresAt: uint64(block.timestamp + 365 days),
            nonce: uint256(keccak256(abi.encode(subject, role)))
        });
    }

    function _sign(uint256 pk, IdentityRegistry.Attestation memory a) internal view returns (bytes memory) {
        (uint8 v, bytes32 r, bytes32 s) = vm.sign(pk, registry.digest(a));
        return abi.encodePacked(r, s, v);
    }

    function _bindLandlord() internal {
        IdentityRegistry.Attestation memory a = _attestation(landlord, LANDLORD_DID, 1, PROPERTY);
        bytes memory sig = _sign(ISSUER_PK, a);
        vm.prank(landlord);
        lease.bindIdentity(a, sig);
    }

    function _bindTenant() internal {
        IdentityRegistry.Attestation memory a = _attestation(tenant, TENANT_DID, 2, bytes32(0));
        bytes memory sig = _sign(ISSUER_PK, a);
        vm.prank(tenant);
        lease.bindIdentity(a, sig);
    }

    function _activate() internal {
        _bindLandlord();
        _bindTenant();
        vm.prank(tenant);
        lease.lockDeposit();
    }

    /// @dev Pays `n` periods, each on its due date.
    function _payPeriods(uint256 n) internal {
        for (uint256 i = 0; i < n; i++) {
            vm.warp(lease.dueDate(i));
            vm.prank(tenant);
            lease.payRent();
        }
    }

    function _toLeaseEnd() internal {
        vm.warp(lease.leaseEnd());
    }
}
