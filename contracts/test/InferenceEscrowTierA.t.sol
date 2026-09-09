// SPDX-License-Identifier: MIT
pragma solidity ^0.8.20;

import {InferenceEscrow} from "../src/InferenceEscrow.sol";
import {InferenceEscrowCollector} from "../src/InferenceEscrowCollector.sol";
import {OperatorRefundCollector} from "../src/OperatorRefundCollector.sol";
import {ReentrantSBC} from "./helpers/MockTokens.sol";
import {EscrowTestBase} from "./helpers/EscrowTestBase.sol";

/// Hardening properties carried over from the v2 baseline (multi-payer
/// isolation, cross-instance replay, malformed signatures, cross-function
/// reentrancy) and re-proven against the v3 auth-capture interface.
contract InferenceEscrowTierATest is EscrowTestBase {
    function setUp() public {
        _deploy();
    }

    // ---- Multi-payer isolation ----

    /// paymentInfoHash is keyed on the full struct including `payer`. Two
    /// payers using the same salt must not collide — the v2 property this
    /// carries forward, now checked against hash identity instead of a nonce map.
    function test_MultiPayerIsolation_SameSaltDifferentPayers() public {
        uint256 aliceKey = 0xA11CE;
        uint256 bobKey = 0xB0B;
        address alice = vm.addr(aliceKey);
        address bob = vm.addr(bobKey);
        _fund(alice, 5_000 * 10 ** 6);
        _fund(bob, 5_000 * 10 ** 6);

        uint256 amount = 1_000 * 10 ** 6;
        InferenceEscrow.PaymentInfo memory infoAlice = _paymentInfo(alice, amount, 0);
        InferenceEscrow.PaymentInfo memory infoBob = _paymentInfo(bob, amount, 0);
        assertTrue(escrow.getHash(infoAlice) != escrow.getHash(infoBob), "payer is part of the hash");

        bytes memory sigAlice = _collectSignature(infoAlice, amount, aliceKey);
        bytes memory sigBob = _collectSignature(infoBob, amount, bobKey);

        vm.prank(operator);
        escrow.charge(infoAlice, amount, address(paymentCollector), _collectorData(sigAlice), 0, address(0));
        vm.prank(operator);
        escrow.charge(infoBob, amount, address(paymentCollector), _collectorData(sigBob), 0, address(0));

        assertEq(escrow.balances(alice), 4_000 * 10 ** 6, "alice debited once");
        assertEq(escrow.balances(bob), 4_000 * 10 ** 6, "bob debited once, not twice");
        assertEq(token.balanceOf(receiver), 2 * amount, "receiver paid for both");
    }

    // ---- Cross-contract / cross-collector replay ----

    /// getHash folds in chainId and `address(this)` (the escrow), so a
    /// signature over one escrow's PaymentInfo can't settle on another
    /// escrow instance — the property the superseded v2 deployment
    /// (0xe629...4d7E, same chain) motivated.
    function test_RevertOnCrossEscrowReplay() public {
        InferenceEscrow escrowB = new InferenceEscrow(address(token));
        InferenceEscrowCollector collectorB = new InferenceEscrowCollector(address(escrowB));
        OperatorRefundCollector refundB = new OperatorRefundCollector(address(escrowB));
        escrowB.setCollectors(address(collectorB), address(refundB));

        uint256 payerKey = 0xA11CE;
        address payer = vm.addr(payerKey);
        _fund(payer, 5_000 * 10 ** 6);
        token.transfer(payer, 5_000 * 10 ** 6);
        vm.prank(payer);
        token.approve(address(escrowB), type(uint256).max);
        vm.prank(payer);
        escrowB.deposit(5_000 * 10 ** 6);

        uint256 amount = 1_000 * 10 ** 6;
        InferenceEscrow.PaymentInfo memory info = _paymentInfo(payer, amount, 0);
        bytes memory sigForA = _collectSignature(info, amount, payerKey); // signed under escrow A's collector domain

        // Same collector CONTRACT exists for B's escrow but at a different
        // address, so getHash(info) computed against escrowB differs from the
        // one signed against escrowA — recovery on B's collector call fails.
        vm.prank(operator);
        vm.expectRevert(); // wrong recovered address, not the payer
        escrowB.authorize(info, amount, address(collectorB), _collectorData(sigForA));

        assertEq(escrowB.balances(payer), 5_000 * 10 ** 6, "escrow B untouched by the replay attempt");
    }

    // ---- Malformed signatures ----

    function test_RevertOnWrongLengthSignature() public {
        uint256 payerKey = 0xA11CE;
        address payer = vm.addr(payerKey);
        _fund(payer, 5_000 * 10 ** 6);

        uint256 amount = 1_000 * 10 ** 6;
        InferenceEscrow.PaymentInfo memory info = _paymentInfo(payer, amount, 0);
        bytes memory sig65 = _collectSignature(info, amount, payerKey);
        bytes memory sig64 = new bytes(64);
        for (uint256 i = 0; i < 64; i++) {
            sig64[i] = sig65[i];
        }

        vm.prank(operator);
        vm.expectRevert(abi.encodeWithSignature("ECDSAInvalidSignatureLength(uint256)", 64));
        escrow.authorize(info, amount, address(paymentCollector), _collectorData(sig64));
    }

    function test_RevertOnInvalidVByte() public {
        uint256 payerKey = 0xA11CE;
        address payer = vm.addr(payerKey);
        _fund(payer, 5_000 * 10 ** 6);

        uint256 amount = 1_000 * 10 ** 6;
        InferenceEscrow.PaymentInfo memory info = _paymentInfo(payer, amount, 0);
        bytes memory sig = _collectSignature(info, amount, payerKey);
        sig[64] = bytes1(uint8(1)); // neither 27 nor 28

        vm.prank(operator);
        vm.expectRevert(abi.encodeWithSignature("ECDSAInvalidSignature()"));
        escrow.authorize(info, amount, address(paymentCollector), _collectorData(sig));
    }

    function test_RevertOnHighSMalleableSignature() public {
        uint256 payerKey = 0xA11CE;
        address payer = vm.addr(payerKey);
        _fund(payer, 5_000 * 10 ** 6);

        uint256 amount = 1_000 * 10 ** 6;
        InferenceEscrow.PaymentInfo memory info = _paymentInfo(payer, amount, 0);
        bytes32 paymentInfoHash = escrow.getHash(info);
        bytes32 structHash =
            keccak256(abi.encode(paymentCollector.COLLECT_TYPEHASH(), paymentInfoHash, amount));
        bytes32 digest = keccak256(abi.encodePacked("\x19\x01", _collectorDomainSeparator(), structHash));
        (uint8 v, bytes32 r, bytes32 s) = vm.sign(payerKey, digest);

        uint256 n = 0xFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFEBAAEDCE6AF48A03BBFD25E8CD0364141;
        bytes32 highS = bytes32(n - uint256(s));
        uint8 flippedV = v == 27 ? 28 : 27;
        bytes memory malleableSig = abi.encodePacked(r, highS, flippedV);

        vm.prank(operator);
        vm.expectRevert(abi.encodeWithSignature("ECDSAInvalidSignatureS(bytes32)", highS));
        escrow.authorize(info, amount, address(paymentCollector), _collectorData(malleableSig));
    }

    // ---- Collector access control ----

    function test_RevertOnCollectTokensCalledDirectlyNotThroughEscrow() public {
        uint256 payerKey = 0xA11CE;
        address payer = vm.addr(payerKey);
        uint256 amount = 1_000 * 10 ** 6;
        InferenceEscrow.PaymentInfo memory info = _paymentInfo(payer, amount, 0);
        bytes memory sig = _collectSignature(info, amount, payerKey);

        vm.expectRevert(InferenceEscrowCollector.OnlyEscrow.selector);
        paymentCollector.collectTokens(info, address(escrow), amount, _collectorData(sig));
    }

    function test_RevertOnWrongCollectorTypeForAuthorize() public {
        uint256 payerKey = 0xA11CE;
        address payer = vm.addr(payerKey);
        _fund(payer, 5_000 * 10 ** 6);
        uint256 amount = 1_000 * 10 ** 6;
        InferenceEscrow.PaymentInfo memory info = _paymentInfo(payer, amount, 0);
        bytes memory sig = _collectSignature(info, amount, payerKey);

        // refundCollector is CollectorType.Refund; authorize() requires Payment.
        vm.prank(operator);
        vm.expectRevert(InferenceEscrow.InvalidCollectorForOperation.selector);
        escrow.authorize(info, amount, address(refundCollector), _collectorData(sig));
    }

    // ---- Edge cases ----

    function test_RevertOnWithdrawWithNoBalance() public {
        address stranger = address(0xD00D);
        vm.prank(stranger);
        vm.expectRevert(
            abi.encodeWithSelector(InferenceEscrow.InsufficientTabBalance.selector, stranger, 0, 1_000)
        );
        escrow.withdraw(1_000);
    }

    function test_DepositZeroIsHarmlessNoOp() public {
        address payer = address(0xCAFE);
        vm.prank(payer);
        escrow.deposit(0);
        assertEq(escrow.balances(payer), 0, "zero deposit credits nothing");
        assertEq(token.balanceOf(address(escrow)), 0, "contract holds nothing");
    }

    function test_RevertOnSetCollectorsCalledTwice() public {
        vm.expectRevert(InferenceEscrow.CollectorsAlreadySet.selector);
        escrow.setCollectors(address(0x1), address(0x2));
    }

    function test_RevertOnSetCollectorsCalledByNonDeployer() public {
        InferenceEscrow freshEscrow = new InferenceEscrow(address(token));
        address stranger = address(0xD00D);

        vm.prank(stranger);
        vm.expectRevert(abi.encodeWithSelector(InferenceEscrow.InvalidSender.selector, stranger, address(this)));
        freshEscrow.setCollectors(address(0x1), address(0x2));
    }

    // ---- Reentrancy ----

    /// nonReentrant is one shared lock across every guarded function. Proven
    /// via a token whose transfer() calls back into a DIFFERENT nonReentrant
    /// function — checks-effects-interactions alone would not catch this,
    /// only the guard does.
    function test_RevertOnCrossFunctionReentrancy_WithdrawIntoDeposit() public {
        ReentrantSBC evilToken = new ReentrantSBC();
        InferenceEscrow evilEscrow = new InferenceEscrow(address(evilToken));

        address payer = address(0xFACE);
        evilToken.transfer(payer, 5_000 * 10 ** 6);
        vm.prank(payer);
        evilToken.approve(address(evilEscrow), type(uint256).max);
        vm.prank(payer);
        evilEscrow.deposit(1_000 * 10 ** 6);

        evilToken.armAttack(address(evilEscrow), abi.encodeWithSelector(InferenceEscrow.deposit.selector, uint256(1)));

        vm.prank(payer);
        vm.expectRevert(abi.encodeWithSignature("ReentrancyGuardReentrantCall()"));
        evilEscrow.withdraw(1_000 * 10 ** 6);
    }

    function test_RevertOnCrossFunctionReentrancy_DepositIntoWithdraw() public {
        ReentrantSBC evilToken = new ReentrantSBC();
        InferenceEscrow evilEscrow = new InferenceEscrow(address(evilToken));

        address payer = address(0xFACE);
        evilToken.transfer(payer, 5_000 * 10 ** 6);
        vm.prank(payer);
        evilToken.approve(address(evilEscrow), type(uint256).max);

        evilToken.armAttack(address(evilEscrow), abi.encodeWithSelector(InferenceEscrow.withdraw.selector, uint256(1)));

        vm.prank(payer);
        vm.expectRevert(abi.encodeWithSignature("ReentrancyGuardReentrantCall()"));
        evilEscrow.deposit(1_000 * 10 ** 6);
    }
}
