// SPDX-License-Identifier: MIT
pragma solidity 0.8.24;

import {EIP712} from "@openzeppelin/contracts/utils/cryptography/EIP712.sol";
import {ECDSA} from "@openzeppelin/contracts/utils/cryptography/ECDSA.sol";
import {Ownable} from "@openzeppelin/contracts/access/Ownable.sol";

/// @title IdentityRegistry
/// @notice On-chain verification layer for identity attestations issued off-chain.
/// @dev Identity *bootstrapping* (checking that a natural person is who they claim to be)
///      remains off-chain and is performed by trusted credential issuers. This contract
///      implements identity *binding*: it verifies that a blockchain address has been
///      attested by a trusted issuer, for a given role and (for landlords) a given property,
///      using EIP-712 typed signatures. Issuers can be added/removed by the registry
///      governor and can revoke attestations they issued.
contract IdentityRegistry is EIP712, Ownable {
    /// @notice Role codes carried inside an attestation.
    uint8 public constant ROLE_LANDLORD = 1;
    uint8 public constant ROLE_TENANT = 2;

    /// @notice An identity attestation signed off-chain by a trusted issuer.
    /// @param subject    the blockchain address being attested
    /// @param didHash    keccak256 of the subject's W3C DID string (the DID itself stays off-chain)
    /// @param role       ROLE_LANDLORD or ROLE_TENANT
    /// @param propertyId property identifier the landlord is attested to control (0 for tenants)
    /// @param expiresAt  unix time after which the attestation is no longer accepted
    /// @param nonce      issuer-chosen value making each attestation unique
    struct Attestation {
        address subject;
        bytes32 didHash;
        uint8 role;
        bytes32 propertyId;
        uint64 expiresAt;
        uint256 nonce;
    }

    bytes32 public constant ATTESTATION_TYPEHASH = keccak256(
        "Attestation(address subject,bytes32 didHash,uint8 role,bytes32 propertyId,uint64 expiresAt,uint256 nonce)"
    );

    mapping(address => bool) public trustedIssuer;
    mapping(bytes32 => bool) public revoked;

    event IssuerAdded(address indexed issuer);
    event IssuerRemoved(address indexed issuer);
    event AttestationRevoked(bytes32 indexed digest, address indexed issuer);

    error ZeroAddress();
    error UntrustedIssuer(address recovered);
    error AttestationExpired();
    error AttestationIsRevoked();
    error NotIssuer();

    constructor(address governor) EIP712("RentalIdentityRegistry", "1") Ownable(governor) {}

    function addIssuer(address issuer) external onlyOwner {
        if (issuer == address(0)) revert ZeroAddress();
        trustedIssuer[issuer] = true;
        emit IssuerAdded(issuer);
    }

    function removeIssuer(address issuer) external onlyOwner {
        trustedIssuer[issuer] = false;
        emit IssuerRemoved(issuer);
    }

    /// @notice EIP-712 digest of an attestation (what the issuer signs).
    function digest(Attestation calldata a) public view returns (bytes32) {
        return _hashTypedDataV4(
            keccak256(
                abi.encode(ATTESTATION_TYPEHASH, a.subject, a.didHash, a.role, a.propertyId, a.expiresAt, a.nonce)
            )
        );
    }

    /// @notice Revokes an attestation. Only the issuer that signed it may revoke it.
    function revoke(Attestation calldata a, bytes calldata signature) external {
        bytes32 d = digest(a);
        if (ECDSA.recover(d, signature) != msg.sender) revert NotIssuer();
        revoked[d] = true;
        emit AttestationRevoked(d, msg.sender);
    }

    /// @notice Verifies signature, issuer trust, expiry and revocation status.
    /// @return issuer the trusted issuer that signed the attestation
    function verify(Attestation calldata a, bytes calldata signature) external view returns (address issuer) {
        bytes32 d = digest(a);
        issuer = ECDSA.recover(d, signature);
        if (!trustedIssuer[issuer]) revert UntrustedIssuer(issuer);
        if (block.timestamp > a.expiresAt) revert AttestationExpired();
        if (revoked[d]) revert AttestationIsRevoked();
    }
}
