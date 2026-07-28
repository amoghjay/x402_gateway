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
contract InferenceEscrow is EIP712, ReentrancyGuard {
    using SafeERC20 for IERC20;

    struct Authorization {
        uint256 amount;
        uint256 nonce;
        uint256 deadline;
    }

    bytes32 public constant AUTHORIZATION_TYPEHASH =
        keccak256("Authorization(uint256 amount,uint256 nonce,uint256 deadline)");

    IERC20 public immutable token;
    address public immutable provider;

    mapping(address => uint256) public balances;
    mapping(address => uint256) public nextNonce;

    event Deposited(address indexed payer, uint256 amount);
    event Settled(address indexed payer, uint256 amount, uint256 nonce);
    event Withdrawn(address indexed payer, uint256 amount);

    constructor(address _token, address _provider) EIP712("InferenceEscrow", "1") {
        token = IERC20(_token);
        provider = _provider;
    }

    function deposit(uint256 amount) external {
        token.safeTransferFrom(msg.sender, address(this), amount);
        balances[msg.sender] += amount;
        emit Deposited(msg.sender, amount);
    }

    /// @notice Settle one prompt's charge. Callable by anyone holding a valid
    /// signature (mirrors Permit2: the signature IS the authorization —
    /// msg.sender here is the gateway's operator wallet, NOT the payer).
    function settle(Authorization calldata auth, bytes calldata signature) external {
        require(block.timestamp <= auth.deadline, "authorization expired");

        bytes32 structHash = keccak256(
            abi.encode(AUTHORIZATION_TYPEHASH, auth.amount, auth.nonce, auth.deadline)
        );
        bytes32 digest = _hashTypedDataV4(structHash);
        address payer = ECDSA.recover(digest, signature);

        require(auth.nonce == nextNonce[payer], "invalid nonce");
        nextNonce[payer] += 1;

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
