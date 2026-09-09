// SPDX-License-Identifier: MIT
pragma solidity ^0.8.20;

import {EIP712} from "@openzeppelin/contracts/utils/cryptography/EIP712.sol";
import {ECDSA} from "@openzeppelin/contracts/utils/cryptography/ECDSA.sol";
import {InferenceEscrow} from "./InferenceEscrow.sol";
import {ITokenCollector} from "./ITokenCollector.sol";

/// @notice The one Payment-type collector InferenceEscrow trusts. Structurally
/// it plays the role base/commerce-payments' PreApprovalPaymentCollector plays
/// for the canonical escrow, but sources funds from the escrow's own
/// deposit-once tab instead of pulling fresh from the payer's wallet — the
/// one place this reimplementation has no canonical counterpart (see
/// InferenceEscrow's divergence #2). `collectTokens` verifies the payer's
/// EIP-712 consent over this exact (paymentInfoHash, amount) and then calls
/// back into the escrow's `debitTab`, which reverts atomically on a bad
/// signature or an insufficient tab — the substitute for canonical's
/// balance-delta assertion.
contract InferenceEscrowCollector is EIP712, ITokenCollector {
    InferenceEscrow public immutable escrow;

    bytes32 public constant COLLECT_TYPEHASH = keccak256("Collect(bytes32 paymentInfoHash,uint256 amount)");

    error OnlyEscrow();
    error InvalidPayerSignature(address recovered, address expected);

    constructor(address escrow_) EIP712("InferenceEscrowCollector", "1") {
        escrow = InferenceEscrow(escrow_);
    }

    /// @inheritdoc ITokenCollector
    function collectorType() external pure override returns (CollectorType) {
        return CollectorType.Payment;
    }

    /// @inheritdoc ITokenCollector
    /// @dev The interface's funds destination is deliberately unnamed here: no
    /// funds move on this path (divergence #2 — the tab is already inside the
    /// escrow), and it would be redundant anyway, since the msg.sender check
    /// below pins the caller to our escrow and that escrow only ever passes
    /// its own address. Same shape as canonical's PreApprovalPaymentCollector,
    /// which likewise leaves its unused parameter nameless.
    /// @param collectorData abi-encoded 65-byte payer signature over
    /// `Collect(paymentInfoHash, amount)` in this contract's own EIP-712 domain.
    function collectTokens(
        InferenceEscrow.PaymentInfo calldata paymentInfo,
        address,
        uint256 amount,
        bytes calldata collectorData
    ) external override {
        if (msg.sender != address(escrow)) revert OnlyEscrow();

        bytes32 paymentInfoHash = escrow.getHash(paymentInfo);
        bytes32 digest = _hashTypedDataV4(keccak256(abi.encode(COLLECT_TYPEHASH, paymentInfoHash, amount)));
        bytes memory signature = abi.decode(collectorData, (bytes));
        address recovered = ECDSA.recover(digest, signature);
        if (recovered != paymentInfo.payer) revert InvalidPayerSignature(recovered, paymentInfo.payer);

        escrow.debitTab(paymentInfo.payer, amount);
    }
}
