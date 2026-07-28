// SPDX-License-Identifier: MIT
pragma solidity ^0.8.20;

import {Test} from "forge-std/Test.sol";
import {InferenceEscrow} from "../src/InferenceEscrow.sol";
import {ERC20} from "@openzeppelin/contracts/token/ERC20/ERC20.sol";

contract MockSBC is ERC20 {
    constructor() ERC20("Mock Stable Coin", "SBC") {
        _mint(msg.sender, 1_000_000 * 10 ** 6);
    }

    function decimals() public pure override returns (uint8) {
        return 6;
    }
}

contract InferenceEscrowTest is Test {
    InferenceEscrow escrow;
    MockSBC token;

    uint256 payerKey = 0xA11CE;
    address payer;
    address provider = address(0xBEEF);

    bytes32 domainSeparator;

    function setUp() public {
        payer = vm.addr(payerKey);
        token = new MockSBC();
        escrow = new InferenceEscrow(address(token), provider);

        // Mirrors what _hashTypedDataV4 does internally — computed independently
        // here rather than relying on the contract, same as we cross-checked the
        // Python signing against a second, independent recovery earlier.
        domainSeparator = keccak256(
            abi.encode(
                keccak256("EIP712Domain(string name,string version,uint256 chainId,address verifyingContract)"),
                keccak256(bytes("InferenceEscrow")),
                keccak256(bytes("1")),
                block.chainid,
                address(escrow)
            )
        );

        token.transfer(payer, 10_000 * 10 ** 6);
        vm.prank(payer);
        token.approve(address(escrow), type(uint256).max);
        vm.prank(payer);
        escrow.deposit(5_000 * 10 ** 6);
    }

    function _sign(uint256 amount, uint256 nonce, uint256 deadline) internal view returns (bytes memory) {
        bytes32 structHash = keccak256(
            abi.encode(escrow.AUTHORIZATION_TYPEHASH(), amount, nonce, deadline)
        );
        bytes32 digest = keccak256(abi.encodePacked("\x19\x01", domainSeparator, structHash));
        (uint8 v, bytes32 r, bytes32 s) = vm.sign(payerKey, digest);
        return abi.encodePacked(r, s, v);
    }

    function test_ValidSettleDebitsAndPaysProvider() public {
        uint256 amount = 1_000 * 10 ** 6;
        uint256 deadline = block.timestamp + 300;
        bytes memory sig = _sign(amount, 0, deadline);

        InferenceEscrow.Authorization memory auth =
            InferenceEscrow.Authorization({amount: amount, nonce: 0, deadline: deadline});

        escrow.settle(auth, sig);

        assertEq(escrow.balances(payer), 4_000 * 10 ** 6, "payer balance should be debited");
        assertEq(token.balanceOf(provider), amount, "provider should be paid");
        assertEq(escrow.nextNonce(payer), 1, "nonce should advance");
    }

    function test_RevertOnReplayedNonce() public {
        uint256 amount = 1_000 * 10 ** 6;
        uint256 deadline = block.timestamp + 300;
        bytes memory sig = _sign(amount, 0, deadline);
        InferenceEscrow.Authorization memory auth =
            InferenceEscrow.Authorization({amount: amount, nonce: 0, deadline: deadline});

        escrow.settle(auth, sig);

        vm.expectRevert("invalid nonce");
        escrow.settle(auth, sig); // same nonce (0) again — replay
    }

    function test_RevertOnExpiredDeadline() public {
        uint256 amount = 1_000 * 10 ** 6;
        uint256 deadline = block.timestamp + 300;
        bytes memory sig = _sign(amount, 0, deadline);
        InferenceEscrow.Authorization memory auth =
            InferenceEscrow.Authorization({amount: amount, nonce: 0, deadline: deadline});

        vm.warp(deadline + 1);
        vm.expectRevert("authorization expired");
        escrow.settle(auth, sig);
    }

    function test_RevertOnInsufficientBalance() public {
        uint256 amount = 6_000 * 10 ** 6; // payer only deposited 5,000
        uint256 deadline = block.timestamp + 300;
        bytes memory sig = _sign(amount, 0, deadline);
        InferenceEscrow.Authorization memory auth =
            InferenceEscrow.Authorization({amount: amount, nonce: 0, deadline: deadline});

        vm.expectRevert("insufficient balance");
        escrow.settle(auth, sig);
    }

    function test_WithdrawAfterPartialSettleReturnsRemainder() public {
        uint256 amount = 1_000 * 10 ** 6;
        uint256 deadline = block.timestamp + 300;
        bytes memory sig = _sign(amount, 0, deadline);
        InferenceEscrow.Authorization memory auth =
            InferenceEscrow.Authorization({amount: amount, nonce: 0, deadline: deadline});
        escrow.settle(auth, sig);

        uint256 payerBalBefore = token.balanceOf(payer);
        vm.prank(payer);
        escrow.withdraw();

        assertEq(escrow.balances(payer), 0, "escrow balance should be zeroed");
        assertEq(token.balanceOf(payer), payerBalBefore + 4_000 * 10 ** 6, "payer should get exact remainder back");
    }
}
