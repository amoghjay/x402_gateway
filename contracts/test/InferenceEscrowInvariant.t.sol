// SPDX-License-Identifier: MIT
pragma solidity ^0.8.20;

import {Test} from "forge-std/Test.sol";
import {StdInvariant} from "forge-std/StdInvariant.sol";
import {InferenceEscrow} from "../src/InferenceEscrow.sol";
import {InferenceEscrowCollector} from "../src/InferenceEscrowCollector.sol";
import {OperatorRefundCollector} from "../src/OperatorRefundCollector.sol";
import {MockSBC} from "./helpers/MockTokens.sol";

/// Drives escrow through fuzzed deposit/authorize/capture/void/charge/withdraw
/// sequences. The invariant grows past Tier A's `balanceOf >= Σ balances` to
/// `balanceOf >= Σ balances + Σ capturable-but-not-yet-captured` — outstanding
/// holds are real liabilities the contract must keep covered too.
contract InferenceEscrowHandler is Test {
    InferenceEscrow public escrow;
    InferenceEscrowCollector public paymentCollector;
    MockSBC public token;
    address public operator;
    address public receiver;

    address[3] public payers;
    uint256[3] private _payerKeys = [uint256(0xA11CE), uint256(0xB0B), uint256(0xC0FFEE)];
    mapping(address => uint256) public nextSalt;

    struct OpenHold {
        InferenceEscrow.PaymentInfo info;
        bytes32 hash;
    }
    OpenHold[] public openHolds;

    constructor(
        InferenceEscrow escrow_,
        InferenceEscrowCollector paymentCollector_,
        MockSBC token_,
        address operator_,
        address receiver_
    ) {
        escrow = escrow_;
        paymentCollector = paymentCollector_;
        token = token_;
        operator = operator_;
        receiver = receiver_;
        for (uint256 i = 0; i < payers.length; i++) {
            payers[i] = vm.addr(_payerKeys[i]);
        }
    }

    function fundPayers() external {
        for (uint256 i = 0; i < payers.length; i++) {
            token.transfer(payers[i], 100_000 * 10 ** 6);
            vm.prank(payers[i]);
            token.approve(address(escrow), type(uint256).max);
        }
    }

    function _payerIndex(uint256 seed) internal pure returns (uint256) {
        return seed % 3;
    }

    function _collectSignature(InferenceEscrow.PaymentInfo memory info, uint256 amount, uint256 payerKey)
        internal
        view
        returns (bytes memory)
    {
        bytes32 paymentInfoHash = escrow.getHash(info);
        bytes32 domainSeparator = keccak256(
            abi.encode(
                keccak256("EIP712Domain(string name,string version,uint256 chainId,address verifyingContract)"),
                keccak256(bytes("InferenceEscrowCollector")),
                keccak256(bytes("1")),
                block.chainid,
                address(paymentCollector)
            )
        );
        bytes32 structHash =
            keccak256(abi.encode(paymentCollector.COLLECT_TYPEHASH(), paymentInfoHash, amount));
        bytes32 digest = keccak256(abi.encodePacked("\x19\x01", domainSeparator, structHash));
        (uint8 v, bytes32 r, bytes32 s) = vm.sign(payerKey, digest);
        return abi.encodePacked(r, s, v);
    }

    function deposit(uint256 payerSeed, uint256 amount) external {
        uint256 idx = _payerIndex(payerSeed);
        address payer = payers[idx];
        amount = bound(amount, 0, token.balanceOf(payer));
        if (amount == 0) return;

        vm.prank(payer);
        escrow.deposit(amount);
    }

    function withdraw(uint256 payerSeed, uint256 amount) external {
        uint256 idx = _payerIndex(payerSeed);
        address payer = payers[idx];
        uint256 tab = escrow.balances(payer);
        if (tab == 0) return;
        amount = bound(amount, 1, tab);

        vm.prank(payer);
        escrow.withdraw(amount);
    }

    /// Single-shot debit-and-pay, no open hold left behind.
    function charge(uint256 payerSeed, uint256 amount) external {
        uint256 idx = _payerIndex(payerSeed);
        address payer = payers[idx];
        uint256 tab = escrow.balances(payer);
        if (tab == 0) return;
        amount = bound(amount, 1, tab);

        InferenceEscrow.PaymentInfo memory info = InferenceEscrow.PaymentInfo({
            operator: operator,
            payer: payer,
            receiver: receiver,
            token: address(token),
            maxAmount: uint120(amount),
            preApprovalExpiry: uint48(block.timestamp + 60),
            authorizationExpiry: uint48(block.timestamp + 300),
            refundExpiry: uint48(block.timestamp + 3600),
            minFeeBps: 0,
            maxFeeBps: 0,
            feeReceiver: address(0),
            salt: nextSalt[payer]++
        });
        bytes memory sig = _collectSignature(info, amount, _payerKeys[idx]);

        vm.prank(operator);
        escrow.charge(info, amount, address(paymentCollector), abi.encode(sig), 0, address(0));
    }

    /// Places a hold and leaves it open — captureOpenHold/voidOpenHold resolve
    /// it later, possibly several handler calls afterward, so the invariant is
    /// actually exercised while capturableAmount > 0 sits on the books.
    function authorize(uint256 payerSeed, uint256 amount) external {
        uint256 idx = _payerIndex(payerSeed);
        address payer = payers[idx];
        uint256 tab = escrow.balances(payer);
        if (tab == 0) return;
        amount = bound(amount, 1, tab);

        InferenceEscrow.PaymentInfo memory info = InferenceEscrow.PaymentInfo({
            operator: operator,
            payer: payer,
            receiver: receiver,
            token: address(token),
            maxAmount: uint120(amount),
            preApprovalExpiry: uint48(block.timestamp + 60),
            authorizationExpiry: uint48(block.timestamp + 300),
            refundExpiry: uint48(block.timestamp + 3600),
            minFeeBps: 0,
            maxFeeBps: 0,
            feeReceiver: address(0),
            salt: nextSalt[payer]++
        });
        bytes memory sig = _collectSignature(info, amount, _payerKeys[idx]);

        vm.prank(operator);
        escrow.authorize(info, amount, address(paymentCollector), abi.encode(sig));
        openHolds.push(OpenHold({info: info, hash: escrow.getHash(info)}));
    }

    function captureOpenHold(uint256 holdSeed, uint256 amountSeed) external {
        if (openHolds.length == 0) return;
        uint256 idx = holdSeed % openHolds.length;
        OpenHold memory hold = openHolds[idx];
        (, uint120 capturable,) = escrow.paymentState(hold.hash);
        if (capturable == 0) return;
        uint256 amount = bound(amountSeed, 1, capturable);

        vm.prank(operator);
        escrow.capture(hold.info, amount, 0, address(0));

        (, uint120 remaining,) = escrow.paymentState(hold.hash);
        if (remaining == 0) _removeHold(idx);
    }

    function voidOpenHold(uint256 holdSeed) external {
        if (openHolds.length == 0) return;
        uint256 idx = holdSeed % openHolds.length;
        OpenHold memory hold = openHolds[idx];
        (, uint120 capturable,) = escrow.paymentState(hold.hash);
        if (capturable == 0) {
            _removeHold(idx);
            return;
        }

        vm.prank(operator);
        escrow.void(hold.info);
        _removeHold(idx);
    }

    function _removeHold(uint256 idx) internal {
        openHolds[idx] = openHolds[openHolds.length - 1];
        openHolds.pop();
    }

    function openHoldsCount() external view returns (uint256) {
        return openHolds.length;
    }

    /// Sum of capturableAmount across every still-open hold, re-read from the
    /// escrow itself rather than trusted from the handler's own bookkeeping.
    function sumOutstandingCapturable() external view returns (uint256 total) {
        for (uint256 i = 0; i < openHolds.length; i++) {
            (, uint120 capturable,) = escrow.paymentState(openHolds[i].hash);
            total += capturable;
        }
    }
}

contract InferenceEscrowInvariantTest is StdInvariant, Test {
    MockSBC token;
    InferenceEscrow escrow;
    InferenceEscrowCollector paymentCollector;
    OperatorRefundCollector refundCollector;
    InferenceEscrowHandler handler;
    address operator = address(0x6A7E);
    address receiver = address(0xBEEF);

    function setUp() public {
        token = new MockSBC();
        escrow = new InferenceEscrow(address(token));
        paymentCollector = new InferenceEscrowCollector(address(escrow));
        refundCollector = new OperatorRefundCollector(address(escrow));
        escrow.setCollectors(address(paymentCollector), address(refundCollector));

        handler = new InferenceEscrowHandler(escrow, paymentCollector, token, operator, receiver);
        token.transfer(address(handler), 300_000 * 10 ** 6);
        handler.fundPayers();

        bytes4[] memory selectors = new bytes4[](6);
        selectors[0] = InferenceEscrowHandler.deposit.selector;
        selectors[1] = InferenceEscrowHandler.withdraw.selector;
        selectors[2] = InferenceEscrowHandler.charge.selector;
        selectors[3] = InferenceEscrowHandler.authorize.selector;
        selectors[4] = InferenceEscrowHandler.captureOpenHold.selector;
        selectors[5] = InferenceEscrowHandler.voidOpenHold.selector;
        targetSelector(FuzzSelector({addr: address(handler), selectors: selectors}));
        targetContract(address(handler));
    }

    /// balanceOf must cover both real liabilities: the deposit-once tabs AND
    /// every outstanding authorized-but-not-yet-captured hold.
    function invariant_ContractHoldsAtLeastWhatItOwes() public view {
        uint256 owed = 0;
        for (uint256 i = 0; i < 3; i++) {
            owed += escrow.balances(handler.payers(i));
        }
        owed += handler.sumOutstandingCapturable();
        assertGe(token.balanceOf(address(escrow)), owed, "escrow insolvent against tabs plus open holds");
    }
}
