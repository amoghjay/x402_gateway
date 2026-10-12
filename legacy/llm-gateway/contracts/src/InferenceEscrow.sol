// SPDX-License-Identifier: MIT
pragma solidity ^0.8.20;

import {IERC20} from "@openzeppelin/contracts/token/ERC20/IERC20.sol";
import {SafeERC20} from "@openzeppelin/contracts/token/ERC20/utils/SafeERC20.sol";
import {EIP712} from "@openzeppelin/contracts/utils/cryptography/EIP712.sol";
import {ECDSA} from "@openzeppelin/contracts/utils/cryptography/ECDSA.sol";
import {ReentrancyGuard} from "@openzeppelin/contracts/utils/ReentrancyGuard.sol";

/// @notice Deposit-once, draw-down-per-prompt settlement for Pay-Per-Prompt.
/// Unlike Permit2, there is no `witness.to` field to sign: this contract is
/// single-tenant (one gateway, one payee), so `provider` is immutable instead.
/// `settler` IS kept (Permit2 calls it `spender`): without it settle() accepts
/// any submitter, so a third party holding a copy of the payer's signature could
/// redeem it, burning the payer's nonce and funds without the payer being served.
contract InferenceEscrow is EIP712, ReentrancyGuard {
    using SafeERC20 for IERC20;

    struct Authorization {
        address settler;
        uint256 amount;
        uint256 nonce;
        uint256 deadline;
    }

    bytes32 public constant AUTHORIZATION_TYPEHASH =
        keccak256("Authorization(address settler,uint256 amount,uint256 nonce,uint256 deadline)");

    IERC20 public immutable token;
    address public immutable provider;

    mapping(address => uint256) public balances;

    /// Unordered nonces: the payer picks any unused number (we use 32 random
    /// bytes) and this records that it is spent. A strictly-incrementing counter
    /// would be cheaper but serialises the payer: two prompts signed before the
    /// first settles would both claim the same nonce, and the second would revert
    /// as a "replay" despite being legitimate. Permit2 solves this with a packed
    /// bitmap; a plain mapping costs more gas but is far simpler to audit.
    mapping(address => mapping(uint256 => bool)) public nonceUsed;

    event Deposited(address indexed payer, uint256 amount);
    event Settled(address indexed payer, uint256 amount, uint256 nonce);
    event Withdrawn(address indexed payer, uint256 amount);

    constructor(address _token, address _provider) EIP712("InferenceEscrow", "1") {
        token = IERC20(_token);
        provider = _provider;
    }

    /// @notice Fund the caller's tab. Credits what actually arrived, not what was
    /// requested: a fee-on-transfer token delivers less than `amount`, and crediting
    /// the request would leave the contract owing more than it holds. Pulling before
    /// crediting is required here (the credit depends on the pull succeeding), so
    /// `nonReentrant` is what closes the gap for a token with transfer hooks.
    function deposit(uint256 amount) external nonReentrant {
        uint256 balanceBefore = token.balanceOf(address(this));
        token.safeTransferFrom(msg.sender, address(this), amount);
        uint256 received = token.balanceOf(address(this)) - balanceBefore;

        balances[msg.sender] += received;
        emit Deposited(msg.sender, received);
    }

    /// @notice Settle one prompt's charge. The signature IS the authorization, so
    /// msg.sender is the gateway's operator wallet, NOT the payer — but it must be
    /// the exact submitter the payer named in `auth.settler`, so only that party
    /// can redeem it.
    function settle(Authorization calldata auth, bytes calldata signature) external {
        require(msg.sender == auth.settler, "unauthorized settler");
        require(block.timestamp <= auth.deadline, "authorization expired");

        bytes32 structHash = keccak256(
            abi.encode(AUTHORIZATION_TYPEHASH, auth.settler, auth.amount, auth.nonce, auth.deadline)
        );
        bytes32 digest = _hashTypedDataV4(structHash);
        address payer = ECDSA.recover(digest, signature);

        require(!nonceUsed[payer][auth.nonce], "nonce already used");
        nonceUsed[payer][auth.nonce] = true;

        require(balances[payer] >= auth.amount, "insufficient balance");
        balances[payer] -= auth.amount;

        token.safeTransfer(provider, auth.amount);
        emit Settled(payer, auth.amount, auth.nonce);
    }

    /// @notice Refund the caller's remaining deposited balance. nonReentrant +
    /// checks-effects-interactions: balance is zeroed BEFORE the external
    /// transfer, since this (unlike settle()) sends value to an
    /// arbitrary caller-controlled address.
    function withdraw() external nonReentrant {
        uint256 amount = balances[msg.sender];
        require(amount > 0, "nothing to withdraw");
        balances[msg.sender] = 0;
        token.safeTransfer(msg.sender, amount);
        emit Withdrawn(msg.sender, amount);
    }
}
