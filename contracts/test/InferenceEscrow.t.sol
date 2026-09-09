// SPDX-License-Identifier: MIT
pragma solidity ^0.8.20;

import {InferenceEscrow} from "../src/InferenceEscrow.sol";
import {InferenceEscrowCollector} from "../src/InferenceEscrowCollector.sol";
import {FeeOnTransferSBC} from "./helpers/MockTokens.sol";
import {EscrowTestBase} from "./helpers/EscrowTestBase.sol";

/// Core auth-capture behaviour: authorize/capture, charge (autoCapture),
/// void, reclaim, refund, and the deposit-once tab underneath all of them.
contract InferenceEscrowTest is EscrowTestBase {
    uint256 payerKey = 0xA11CE;
    address payer;

    function setUp() public {
        _deploy();
        payer = vm.addr(payerKey);
        _fund(payer, 5_000 * 10 ** 6);
    }

    function test_AuthorizeThenCaptureDebitsTabAndPaysReceiver() public {
        uint256 amount = 1_000 * 10 ** 6;
        InferenceEscrow.PaymentInfo memory info = _paymentInfo(payer, amount, 0);
        bytes memory sig = _collectSignature(info, amount, payerKey);

        vm.prank(operator);
        escrow.authorize(info, amount, address(paymentCollector), _collectorData(sig));

        assertEq(escrow.balances(payer), 4_000 * 10 ** 6, "tab debited on authorize");
        assertEq(token.balanceOf(receiver), 0, "receiver not paid until capture");

        vm.prank(operator);
        escrow.capture(info, amount, 0, address(0));

        assertEq(token.balanceOf(receiver), amount, "receiver paid on capture");
        bytes32 hash = escrow.getHash(info);
        (bool collected, uint120 capturable, uint120 refundable) = escrow.paymentState(hash);
        assertTrue(collected);
        assertEq(capturable, 0, "hold fully captured");
        assertEq(refundable, amount, "captured amount becomes refundable");
    }

    /// The racy control: one call debits the tab and pays receiver, no hold.
    function test_ChargeAutoCaptureDebitsTabAndPaysReceiverInOneTx() public {
        uint256 amount = 1_000 * 10 ** 6;
        InferenceEscrow.PaymentInfo memory info = _paymentInfo(payer, amount, 0);
        bytes memory sig = _collectSignature(info, amount, payerKey);

        vm.prank(operator);
        escrow.charge(info, amount, address(paymentCollector), _collectorData(sig), 0, address(0));

        assertEq(escrow.balances(payer), 4_000 * 10 ** 6, "tab debited");
        assertEq(token.balanceOf(receiver), amount, "receiver paid immediately");
    }

    function test_RevertOnAuthorizeWithWrongPayerSignature() public {
        uint256 amount = 1_000 * 10 ** 6;
        InferenceEscrow.PaymentInfo memory info = _paymentInfo(payer, amount, 0);
        uint256 strangerKey = 0xBAD;
        bytes memory wrongSig = _collectSignature(info, amount, strangerKey);

        vm.prank(operator);
        vm.expectRevert(
            abi.encodeWithSelector(InferenceEscrowCollector.InvalidPayerSignature.selector, vm.addr(strangerKey), payer)
        );
        escrow.authorize(info, amount, address(paymentCollector), _collectorData(wrongSig));

        assertEq(escrow.balances(payer), 5_000 * 10 ** 6, "tab untouched on rejected consent");
    }

    function test_RevertOnPaymentAlreadyCollected() public {
        uint256 amount = 1_000 * 10 ** 6;
        InferenceEscrow.PaymentInfo memory info = _paymentInfo(payer, amount, 0);
        bytes memory sig = _collectSignature(info, amount, payerKey);

        vm.prank(operator);
        escrow.authorize(info, amount, address(paymentCollector), _collectorData(sig));

        // Computed before pranking: escrow.getHash(info) is itself an external
        // call, and vm.prank only covers the very next one — it would be spent
        // here, not on authorize(), same trap the v2 tests already noted.
        bytes32 hash = escrow.getHash(info);
        vm.prank(operator);
        vm.expectRevert(abi.encodeWithSelector(InferenceEscrow.PaymentAlreadyCollected.selector, hash));
        escrow.authorize(info, amount, address(paymentCollector), _collectorData(sig));
    }

    function test_VoidRecreditsTab() public {
        uint256 amount = 1_000 * 10 ** 6;
        InferenceEscrow.PaymentInfo memory info = _paymentInfo(payer, amount, 0);
        bytes memory sig = _collectSignature(info, amount, payerKey);

        vm.prank(operator);
        escrow.authorize(info, amount, address(paymentCollector), _collectorData(sig));
        assertEq(escrow.balances(payer), 4_000 * 10 ** 6, "held");

        vm.prank(operator);
        escrow.void(info);

        assertEq(escrow.balances(payer), 5_000 * 10 ** 6, "released hold re-credited to the tab, not the EOA");
        assertEq(token.balanceOf(payer), 0, "no external payout happened");
    }

    function test_RevertOnReclaimBeforeAuthorizationExpiry() public {
        uint256 amount = 1_000 * 10 ** 6;
        InferenceEscrow.PaymentInfo memory info = _paymentInfo(payer, amount, 0);
        bytes memory sig = _collectSignature(info, amount, payerKey);
        vm.prank(operator);
        escrow.authorize(info, amount, address(paymentCollector), _collectorData(sig));

        vm.prank(payer);
        vm.expectRevert(
            abi.encodeWithSelector(
                InferenceEscrow.BeforeAuthorizationExpiry.selector, uint48(block.timestamp), info.authorizationExpiry
            )
        );
        escrow.reclaim(info);
    }

    function test_ReclaimAfterAuthorizationExpiryRecreditsTab() public {
        uint256 amount = 1_000 * 10 ** 6;
        InferenceEscrow.PaymentInfo memory info = _paymentInfo(payer, amount, 0);
        bytes memory sig = _collectSignature(info, amount, payerKey);
        vm.prank(operator);
        escrow.authorize(info, amount, address(paymentCollector), _collectorData(sig));

        vm.warp(info.authorizationExpiry);
        vm.prank(payer);
        escrow.reclaim(info);

        assertEq(escrow.balances(payer), 5_000 * 10 ** 6, "payer self-served the release back into the tab");
    }

    function test_RevertOnCaptureAfterAuthorizationExpiry() public {
        uint256 amount = 1_000 * 10 ** 6;
        InferenceEscrow.PaymentInfo memory info = _paymentInfo(payer, amount, 0);
        bytes memory sig = _collectSignature(info, amount, payerKey);
        vm.prank(operator);
        escrow.authorize(info, amount, address(paymentCollector), _collectorData(sig));

        vm.warp(info.authorizationExpiry);
        vm.prank(operator);
        vm.expectRevert(
            abi.encodeWithSelector(
                InferenceEscrow.AfterAuthorizationExpiry.selector, uint48(block.timestamp), info.authorizationExpiry
            )
        );
        escrow.capture(info, amount, 0, address(0));
    }

    function test_PartialCaptureThenVoidRemainder() public {
        uint256 maxAmount = 1_000 * 10 ** 6;
        InferenceEscrow.PaymentInfo memory info = _paymentInfo(payer, maxAmount, 0);
        bytes memory sig = _collectSignature(info, maxAmount, payerKey);
        vm.prank(operator);
        escrow.authorize(info, maxAmount, address(paymentCollector), _collectorData(sig));

        uint256 partialAmount = 400 * 10 ** 6;
        vm.prank(operator);
        escrow.capture(info, partialAmount, 0, address(0));
        assertEq(token.balanceOf(receiver), partialAmount, "partial capture paid");

        vm.prank(operator);
        escrow.void(info);
        assertEq(
            escrow.balances(payer), 5_000 * 10 ** 6 - partialAmount, "only the captured share left the tab permanently"
        );
    }

    function test_RefundPullsOperatorLiquidityAndPaysPayer() public {
        uint256 amount = 1_000 * 10 ** 6;
        InferenceEscrow.PaymentInfo memory info = _paymentInfo(payer, amount, 0);
        bytes memory sig = _collectSignature(info, amount, payerKey);
        vm.prank(operator);
        escrow.charge(info, amount, address(paymentCollector), _collectorData(sig), 0, address(0));
        assertEq(token.balanceOf(receiver), amount);

        _approveOperatorRefundLiquidity(amount);

        vm.prank(operator);
        escrow.refund(info, amount, address(refundCollector), "");

        assertEq(token.balanceOf(payer), amount, "refund paid the payer's EOA directly, from fresh liquidity");
        (, , uint120 refundable) = escrow.paymentState(escrow.getHash(info));
        assertEq(refundable, 0, "refundable balance consumed");
    }

    function test_RevertOnRefundAfterRefundExpiry() public {
        uint256 amount = 1_000 * 10 ** 6;
        InferenceEscrow.PaymentInfo memory info = _paymentInfo(payer, amount, 0);
        bytes memory sig = _collectSignature(info, amount, payerKey);
        vm.prank(operator);
        escrow.charge(info, amount, address(paymentCollector), _collectorData(sig), 0, address(0));
        _approveOperatorRefundLiquidity(amount);

        vm.warp(info.refundExpiry);
        vm.prank(operator);
        vm.expectRevert(
            abi.encodeWithSelector(InferenceEscrow.AfterRefundExpiry.selector, uint48(block.timestamp), info.refundExpiry)
        );
        escrow.refund(info, amount, address(refundCollector), "");
    }

    function test_RevertOnExceedsMaxAmount() public {
        uint256 maxAmount = 1_000 * 10 ** 6;
        InferenceEscrow.PaymentInfo memory info = _paymentInfo(payer, maxAmount, 0);
        uint256 tooMuch = maxAmount + 1;
        bytes memory sig = _collectSignature(info, tooMuch, payerKey);

        vm.prank(operator);
        vm.expectRevert(abi.encodeWithSelector(InferenceEscrow.ExceedsMaxAmount.selector, tooMuch, maxAmount));
        escrow.authorize(info, tooMuch, address(paymentCollector), _collectorData(sig));
    }

    function test_RevertOnInvalidExpiryOrdering() public {
        InferenceEscrow.PaymentInfo memory info = _paymentInfo(payer, 1_000 * 10 ** 6, 0);
        info.authorizationExpiry = info.preApprovalExpiry - 1; // authorization before preApproval: invalid
        bytes memory sig = _collectSignature(info, 1_000 * 10 ** 6, payerKey);

        vm.prank(operator);
        vm.expectRevert(
            abi.encodeWithSelector(
                InferenceEscrow.InvalidExpiries.selector,
                info.preApprovalExpiry,
                info.authorizationExpiry,
                info.refundExpiry
            )
        );
        escrow.authorize(info, 1_000 * 10 ** 6, address(paymentCollector), _collectorData(sig));
    }

    function test_RevertOnAfterPreApprovalExpiry() public {
        InferenceEscrow.PaymentInfo memory info = _paymentInfo(payer, 1_000 * 10 ** 6, 0);
        bytes memory sig = _collectSignature(info, 1_000 * 10 ** 6, payerKey);

        vm.warp(info.preApprovalExpiry);
        vm.prank(operator);
        vm.expectRevert(
            abi.encodeWithSelector(
                InferenceEscrow.AfterPreApprovalExpiry.selector, uint48(block.timestamp), info.preApprovalExpiry
            )
        );
        escrow.authorize(info, 1_000 * 10 ** 6, address(paymentCollector), _collectorData(sig));
    }

    function test_WithdrawPartial() public {
        vm.prank(payer);
        escrow.withdraw(1_000 * 10 ** 6);

        assertEq(escrow.balances(payer), 4_000 * 10 ** 6, "partial withdrawal leaves the remainder in the tab");
        assertEq(token.balanceOf(payer), 1_000 * 10 ** 6);
    }

    function test_RevertOnWithdrawMoreThanBalance() public {
        vm.prank(payer);
        vm.expectRevert(
            abi.encodeWithSelector(InferenceEscrow.InsufficientTabBalance.selector, payer, 5_000 * 10 ** 6, 6_000 * 10 ** 6)
        );
        escrow.withdraw(6_000 * 10 ** 6);
    }

    /// deposit() must credit what ARRIVED, not what was asked for. Crediting the
    /// requested amount against a fee-on-transfer token would leave the contract
    /// owing more than it holds. Unchanged behaviour from the prior design.
    function test_DepositCreditsAmountActuallyReceived() public {
        FeeOnTransferSBC feeToken = new FeeOnTransferSBC();
        InferenceEscrow feeEscrow = new InferenceEscrow(address(feeToken));

        feeToken.transfer(payer, 10_000 * 10 ** 6);
        vm.startPrank(payer);
        feeToken.approve(address(feeEscrow), type(uint256).max);

        uint256 requested = 1_000 * 10 ** 6;
        feeEscrow.deposit(requested);
        vm.stopPrank();

        uint256 credited = feeEscrow.balances(payer);
        assertEq(credited, requested - requested / 100, "credited the 99% that arrived");
        assertEq(feeToken.balanceOf(address(feeEscrow)), credited, "contract is solvent: holds exactly what it owes");

        vm.prank(payer);
        feeEscrow.withdraw(credited);
        assertEq(feeEscrow.balances(payer), 0, "tab drained");
    }
}
