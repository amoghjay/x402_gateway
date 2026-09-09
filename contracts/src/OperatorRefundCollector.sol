// SPDX-License-Identifier: MIT
pragma solidity ^0.8.20;

import {IERC20} from "@openzeppelin/contracts/token/ERC20/IERC20.sol";
import {SafeERC20} from "@openzeppelin/contracts/token/ERC20/utils/SafeERC20.sol";
import {InferenceEscrow} from "./InferenceEscrow.sol";
import {ITokenCollector} from "./ITokenCollector.sol";

/// @notice Near-verbatim port of base/commerce-payments' OperatorRefundCollector
/// — no divergence here (see InferenceEscrow.refund's docstring): capture()
/// already paid `receiver` out, so a refund needs fresh liquidity, pulled from
/// the operator's own pre-approved allowance and forwarded to the escrow.
contract OperatorRefundCollector is ITokenCollector {
    InferenceEscrow public immutable escrow;

    error OnlyEscrow();

    constructor(address escrow_) {
        escrow = InferenceEscrow(escrow_);
    }

    /// @inheritdoc ITokenCollector
    function collectorType() external pure override returns (CollectorType) {
        return CollectorType.Refund;
    }

    /// @inheritdoc ITokenCollector
    /// @dev collectorData is unused: unlike the payment path, refund funding
    /// carries no payer consent to check — the operator is msg.sender-gated by
    /// InferenceEscrow.refund() before this is ever called.
    function collectTokens(
        InferenceEscrow.PaymentInfo calldata paymentInfo,
        address escrow_,
        uint256 amount,
        bytes calldata
    ) external override {
        if (msg.sender != address(escrow)) revert OnlyEscrow();
        SafeERC20.safeTransferFrom(IERC20(paymentInfo.token), paymentInfo.operator, escrow_, amount);
    }
}
