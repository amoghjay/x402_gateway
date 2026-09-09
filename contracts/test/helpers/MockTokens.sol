// SPDX-License-Identifier: MIT
pragma solidity ^0.8.20;

import {ERC20} from "@openzeppelin/contracts/token/ERC20/ERC20.sol";

/// Shared across Tier A and the original settle() tests so both suites see the
/// same token behaviour rather than two mocks drifting apart.
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

/// @notice A token that calls back into an arbitrary target from inside `transfer`
/// and `transferFrom`, once per call, to prove escrow's nonReentrant guard blocks
/// cross-function reentrancy — not just same-function replay, which
/// checks-effects-interactions already rules out on its own.
contract ReentrantSBC is ERC20 {
    address public attackTarget;
    bytes public attackPayload;
    bool private _attacking;

    constructor() ERC20("Reentrant SBC", "rSBC") {
        _mint(msg.sender, 1_000_000 * 10 ** 6);
    }

    function decimals() public pure override returns (uint8) {
        return 6;
    }

    function armAttack(address target, bytes calldata payload) external {
        attackTarget = target;
        attackPayload = payload;
    }

    function _reenter() private {
        if (_attacking || attackTarget == address(0)) return;
        _attacking = true;
        // Deliberately ignore success: the point is that the guarded call
        // reverts, and callers assert on THAT revert bubbling up through here.
        (bool ok, bytes memory ret) = attackTarget.call(attackPayload);
        if (!ok) {
            assembly ("memory-safe") {
                revert(add(ret, 32), mload(ret))
            }
        }
        _attacking = false;
    }

    function transfer(address to, uint256 value) public override returns (bool) {
        _reenter();
        return super.transfer(to, value);
    }

    function transferFrom(address from, address to, uint256 value) public override returns (bool) {
        _reenter();
        return super.transferFrom(from, to, value);
    }
}
