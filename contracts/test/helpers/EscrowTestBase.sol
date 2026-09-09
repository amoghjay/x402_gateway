// SPDX-License-Identifier: MIT
pragma solidity ^0.8.20;

import {Test} from "forge-std/Test.sol";
import {InferenceEscrow} from "../../src/InferenceEscrow.sol";
import {InferenceEscrowCollector} from "../../src/InferenceEscrowCollector.sol";
import {OperatorRefundCollector} from "../../src/OperatorRefundCollector.sol";
import {MockSBC} from "./MockTokens.sol";

/// Shared deploy + signing helpers for the v3 (auth-capture) test suites, so
/// each suite isn't re-deriving the collector's EIP-712 domain by hand.
contract EscrowTestBase is Test {
    MockSBC token;
    InferenceEscrow escrow;
    InferenceEscrowCollector paymentCollector;
    OperatorRefundCollector refundCollector;

    address operator = address(0x6A7E);
    address receiver = address(0xBEEF);

    function _deploy() internal {
        token = new MockSBC();
        escrow = new InferenceEscrow(address(token));
        paymentCollector = new InferenceEscrowCollector(address(escrow));
        refundCollector = new OperatorRefundCollector(address(escrow));
        escrow.setCollectors(address(paymentCollector), address(refundCollector));
    }

    function _paymentInfo(address payer, uint256 maxAmount, uint256 salt)
        internal
        view
        returns (InferenceEscrow.PaymentInfo memory)
    {
        return InferenceEscrow.PaymentInfo({
            operator: operator,
            payer: payer,
            receiver: receiver,
            token: address(token),
            maxAmount: uint120(maxAmount),
            preApprovalExpiry: uint48(block.timestamp + 60),
            authorizationExpiry: uint48(block.timestamp + 300),
            refundExpiry: uint48(block.timestamp + 3600),
            minFeeBps: 0,
            maxFeeBps: 0,
            feeReceiver: address(0),
            salt: salt
        });
    }

    function _fund(address payer, uint256 amount) internal {
        token.transfer(payer, amount);
        vm.prank(payer);
        token.approve(address(escrow), type(uint256).max);
        vm.prank(payer);
        escrow.deposit(amount);
    }

    /// Give the operator its own tokens and a standing approval to the refund
    /// collector — the liquidity path `refund()` pulls from, mirroring
    /// OperatorRefundCollector's real allowance requirement.
    function _approveOperatorRefundLiquidity(uint256 amount) internal {
        token.transfer(operator, amount);
        vm.prank(operator);
        token.approve(address(refundCollector), amount);
    }

    function _collectorDomainSeparator() internal view returns (bytes32) {
        return keccak256(
            abi.encode(
                keccak256("EIP712Domain(string name,string version,uint256 chainId,address verifyingContract)"),
                keccak256(bytes("InferenceEscrowCollector")),
                keccak256(bytes("1")),
                block.chainid,
                address(paymentCollector)
            )
        );
    }

    /// The payer's consent signature over (paymentInfoHash, amount), in the
    /// collector's own EIP-712 domain.
    function _collectSignature(InferenceEscrow.PaymentInfo memory info, uint256 amount, uint256 payerKey)
        internal
        view
        returns (bytes memory signature)
    {
        bytes32 paymentInfoHash = escrow.getHash(info);
        bytes32 structHash =
            keccak256(abi.encode(paymentCollector.COLLECT_TYPEHASH(), paymentInfoHash, amount));
        bytes32 digest = keccak256(abi.encodePacked("\x19\x01", _collectorDomainSeparator(), structHash));
        (uint8 v, bytes32 r, bytes32 s) = vm.sign(payerKey, digest);
        signature = abi.encodePacked(r, s, v);
    }

    function _collectorData(bytes memory signature) internal pure returns (bytes memory) {
        return abi.encode(signature);
    }
}
