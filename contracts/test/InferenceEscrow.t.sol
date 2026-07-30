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

/// Burns 1% on every transfer, so the recipient receives less than was sent.
contract FeeOnTransferSBC is ERC20 {
    constructor() ERC20("Fee SBC", "fSBC") {
        _mint(msg.sender, 1_000_000 * 10 ** 6);
    }

    function decimals() public pure override returns (uint8) {
        return 6;
    }

    function _update(address from, address to, uint256 value) internal override {
        if (from != address(0) && to != address(0)) {
            uint256 fee = value / 100;
            super._update(from, address(0xDEAD), fee);
            value -= fee;
        }
        super._update(from, to, value);
    }
}

contract InferenceEscrowTest is Test {
    InferenceEscrow escrow;
    MockSBC token;

    uint256 payerKey = 0xA11CE;
    address payer;
    address provider = address(0xBEEF);
    address settler = address(0x6A7E);   // the gateway operator wallet
    address stranger = address(0xBAD);

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

    function _sign(address settler_, uint256 amount, uint256 nonce, uint256 deadline)
        internal
        view
        returns (bytes memory)
    {
        bytes32 structHash = keccak256(
            abi.encode(escrow.AUTHORIZATION_TYPEHASH(), settler_, amount, nonce, deadline)
        );
        bytes32 digest = keccak256(abi.encodePacked("\x19\x01", domainSeparator, structHash));
        (uint8 v, bytes32 r, bytes32 s) = vm.sign(payerKey, digest);
        return abi.encodePacked(r, s, v);
    }

    /// Build the matching auth struct for a signature made by _sign.
    function _auth(address settler_, uint256 amount, uint256 nonce, uint256 deadline)
        internal
        pure
        returns (InferenceEscrow.Authorization memory)
    {
        return InferenceEscrow.Authorization({
            settler: settler_,
            amount: amount,
            nonce: nonce,
            deadline: deadline
        });
    }

    function test_ValidSettleDebitsAndPaysProvider() public {
        uint256 amount = 1_000 * 10 ** 6;
        uint256 deadline = block.timestamp + 300;
        bytes memory sig = _sign(settler, amount, 0, deadline);

        vm.prank(settler);
        escrow.settle(_auth(settler, amount, 0, deadline), sig);

        assertEq(escrow.balances(payer), 4_000 * 10 ** 6, "payer balance should be debited");
        assertEq(token.balanceOf(provider), amount, "provider should be paid");
        assertTrue(escrow.nonceUsed(payer, 0), "nonce should be marked spent");
    }

    /// The payer names the gateway as settler; a stranger who obtained a copy of
    /// the signature must not be able to redeem it (burning the payer's nonce and
    /// funds without the payer ever being served).
    function test_RevertOnUnauthorizedSettler() public {
        uint256 amount = 1_000 * 10 ** 6;
        uint256 deadline = block.timestamp + 300;
        bytes memory sig = _sign(settler, amount, 0, deadline);
        InferenceEscrow.Authorization memory auth = _auth(settler, amount, 0, deadline);

        vm.prank(stranger);
        vm.expectRevert("unauthorized settler");
        escrow.settle(auth, sig);

        // Nothing was consumed: the authorization is still spendable by the gateway.
        assertFalse(escrow.nonceUsed(payer, 0), "nonce must not be spent on a rejected settle");
        assertEq(escrow.balances(payer), 5_000 * 10 ** 6, "balance must be untouched");

        vm.prank(settler);
        escrow.settle(auth, sig);
        assertTrue(escrow.nonceUsed(payer, 0), "gateway can still settle afterwards");
    }

    /// A stranger cannot escape the check by rewriting `settler` in the calldata:
    /// that field is signed, so the digest changes and recovery yields a different
    /// address than the payer.
    function test_RevertWhenStrangerRewritesSettlerField() public {
        uint256 amount = 1_000 * 10 ** 6;
        uint256 deadline = block.timestamp + 300;
        bytes memory sig = _sign(settler, amount, 0, deadline);

        vm.prank(stranger);
        vm.expectRevert("insufficient balance"); // recovered address is not the payer
        escrow.settle(_auth(stranger, amount, 0, deadline), sig);

        assertEq(escrow.balances(payer), 5_000 * 10 ** 6, "payer must be untouched");
    }

    function test_RevertOnReplayedNonce() public {
        uint256 amount = 1_000 * 10 ** 6;
        uint256 deadline = block.timestamp + 300;
        bytes memory sig = _sign(settler, amount, 0, deadline);
        InferenceEscrow.Authorization memory auth = _auth(settler, amount, 0, deadline);

        vm.prank(settler);
        escrow.settle(auth, sig);

        vm.prank(settler);
        vm.expectRevert("nonce already used");
        escrow.settle(auth, sig); // same nonce (0) again — replay
    }

    /// The point of unordered nonces: arbitrary, non-sequential values settle fine
    /// and in any order, so concurrent prompts from one payer can't collide.
    function test_UnorderedNoncesSettleInAnyOrder() public {
        uint256 amount = 100 * 10 ** 6;
        uint256 deadline = block.timestamp + 300;
        uint256[3] memory nonces = [type(uint256).max, uint256(7), 0x9e2f];

        for (uint256 i = 0; i < nonces.length; i++) {
            // Build the signature BEFORE pranking: _sign reads
            // escrow.AUTHORIZATION_TYPEHASH(), and vm.prank only covers the very
            // next external call — it would be spent on that read, not on settle().
            bytes memory sig = _sign(settler, amount, nonces[i], deadline);
            vm.prank(settler);
            escrow.settle(_auth(settler, amount, nonces[i], deadline), sig);
            assertTrue(escrow.nonceUsed(payer, nonces[i]), "each nonce marked spent");
        }

        assertEq(escrow.balances(payer), 5_000 * 10 ** 6 - 3 * amount, "all three charged");
        assertEq(token.balanceOf(provider), 3 * amount, "provider paid three times");
    }

    function test_RevertOnExpiredDeadline() public {
        uint256 amount = 1_000 * 10 ** 6;
        uint256 deadline = block.timestamp + 300;
        bytes memory sig = _sign(settler, amount, 0, deadline);

        vm.warp(deadline + 1);
        vm.prank(settler);
        vm.expectRevert("authorization expired");
        escrow.settle(_auth(settler, amount, 0, deadline), sig);
    }

    function test_RevertOnInsufficientBalance() public {
        uint256 amount = 6_000 * 10 ** 6; // payer only deposited 5,000
        uint256 deadline = block.timestamp + 300;
        bytes memory sig = _sign(settler, amount, 0, deadline);

        vm.prank(settler);
        vm.expectRevert("insufficient balance");
        escrow.settle(_auth(settler, amount, 0, deadline), sig);
    }

    /// deposit() must credit what ARRIVED, not what was asked for. Crediting the
    /// requested amount against a fee-on-transfer token would leave the contract
    /// owing more than it holds, and the last withdrawer would be unable to exit.
    function test_DepositCreditsAmountActuallyReceived() public {
        FeeOnTransferSBC feeToken = new FeeOnTransferSBC();
        InferenceEscrow feeEscrow = new InferenceEscrow(address(feeToken), provider);

        feeToken.transfer(payer, 10_000 * 10 ** 6);
        vm.startPrank(payer);
        feeToken.approve(address(feeEscrow), type(uint256).max);

        uint256 requested = 1_000 * 10 ** 6;
        feeEscrow.deposit(requested);
        vm.stopPrank();

        uint256 credited = feeEscrow.balances(payer);
        assertEq(credited, requested - requested / 100, "credited the 99% that arrived");
        assertEq(feeToken.balanceOf(address(feeEscrow)), credited, "contract is solvent: holds exactly what it owes");

        // And the payer can actually get it all back out.
        vm.prank(payer);
        feeEscrow.withdraw();
        assertEq(feeEscrow.balances(payer), 0, "tab drained");
    }

    function test_WithdrawAfterPartialSettleReturnsRemainder() public {
        uint256 amount = 1_000 * 10 ** 6;
        uint256 deadline = block.timestamp + 300;
        bytes memory sig = _sign(settler, amount, 0, deadline);
        vm.prank(settler);
        escrow.settle(_auth(settler, amount, 0, deadline), sig);

        uint256 payerBalBefore = token.balanceOf(payer);
        vm.prank(payer);
        escrow.withdraw();

        assertEq(escrow.balances(payer), 0, "escrow balance should be zeroed");
        assertEq(token.balanceOf(payer), payerBalBefore + 4_000 * 10 ** 6, "payer should get exact remainder back");
    }
}
