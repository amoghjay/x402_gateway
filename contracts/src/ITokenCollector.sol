// SPDX-License-Identifier: MIT
pragma solidity ^0.8.20;

import {InferenceEscrow} from "./InferenceEscrow.sol";

/// @notice Minimal local re-statement of base/commerce-payments' TokenCollector
/// ABI (github.com/base/commerce-payments/blob/main/src/collectors/TokenCollector.sol).
/// Vendoring that whole repo as a submodule for one interface was judged not
/// worth the dependency; the SHAPE is what conformance is measured against,
/// and every collector that plugs into this escrow is authored here, so the
/// canonical implementation itself is not needed.
interface ITokenCollector {
    enum CollectorType {
        Payment,
        Refund
    }

    function collectorType() external view returns (CollectorType);

    /// @param paymentInfo The payment being collected for
    /// @param escrow The InferenceEscrow to deliver funds to (canonical calls this `tokenStore`;
    ///        we have no per-operator store, so it is always the escrow itself)
    /// @param amount Amount to collect
    /// @param collectorData Collector-specific proof of payer consent
    function collectTokens(
        InferenceEscrow.PaymentInfo calldata paymentInfo,
        address escrow,
        uint256 amount,
        bytes calldata collectorData
    ) external;
}
