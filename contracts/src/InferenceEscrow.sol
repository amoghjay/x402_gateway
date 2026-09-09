// SPDX-License-Identifier: MIT
pragma solidity ^0.8.20;

import {IERC20} from "@openzeppelin/contracts/token/ERC20/IERC20.sol";
import {SafeERC20} from "@openzeppelin/contracts/token/ERC20/utils/SafeERC20.sol";
import {ReentrancyGuard} from "@openzeppelin/contracts/utils/ReentrancyGuard.sol";
import {ITokenCollector} from "./ITokenCollector.sol";

/// @notice An implementation of the x402 `auth-capture` scheme
/// (specs/schemes/auth-capture/scheme_auth_capture_evm.md), ported closely from
/// the reference `AuthCaptureEscrow` in base/commerce-payments. Three places
/// deliberately diverge from that reference, each because this escrow keeps a
/// deposit-once payer tab that the canonical design does not have:
///
///   1. No per-operator TokenStore cloning. Canonical isolates each operator's
///      funds in its own CREATE2-cloned store; this escrow has exactly one
///      operator (the gateway), so that isolation is moot and funds sit
///      directly in this contract, as in the prior deposit-once design.
///   2. Collection is a tab-debit-or-revert, not an observed balance delta.
///      Canonical's collectors pull fresh funds from the payer's wallet and
///      the escrow asserts the token store's balance grew by exactly `amount`.
///      Deposit-once payers already funded this contract at deposit() time, so
///      there is no external transfer for authorize()/charge() to observe —
///      see InferenceEscrowCollector for the atomic substitute.
///   3. void()/reclaim() re-credit the payer's tab instead of paying their EOA
///      directly, so a released hold is available for the very next prompt
///      with no extra on-chain step.
///
/// refund() has no divergence: capture() already pays `receiver` immediately
/// (as settle() always has), so a post-capture refund needs fresh liquidity
/// exactly as canonical assumes, pulled from the operator's own allowance.
contract InferenceEscrow is ReentrancyGuard {
    using SafeERC20 for IERC20;

    /// @notice All information required to authorize and capture a unique payment.
    /// Field order and types match AuthCaptureEscrow.PaymentInfo exactly — this
    /// is the literal conformance artifact everything else is checked against.
    struct PaymentInfo {
        address operator;
        address payer;
        address receiver;
        address token;
        uint120 maxAmount;
        uint48 preApprovalExpiry;
        uint48 authorizationExpiry;
        uint48 refundExpiry;
        uint16 minFeeBps;
        uint16 maxFeeBps;
        address feeReceiver;
        uint256 salt;
    }

    struct PaymentState {
        bool hasCollectedPayment;
        uint120 capturableAmount;
        uint120 refundableAmount;
    }

    bytes32 public constant PAYMENT_INFO_TYPEHASH = keccak256(
        "PaymentInfo(address operator,address payer,address receiver,address token,uint120 maxAmount,uint48 preApprovalExpiry,uint48 authorizationExpiry,uint48 refundExpiry,uint16 minFeeBps,uint16 maxFeeBps,address feeReceiver,uint256 salt)"
    );

    uint16 internal constant _MAX_FEE_BPS = 10_000;

    IERC20 public immutable token;

    /// @notice Whoever deployed this contract — the only address allowed to
    /// call `setCollectors`, once. Without this gate, a public-testnet deploy
    /// has a window between construction and wiring where anyone could race
    /// the deploy script and register themselves as the trusted collector.
    address public immutable deployer;

    /// @notice The one Payment-type collector trusted to call `debitTab`.
    address public paymentCollector;
    /// @notice The one Refund-type collector trusted to fund refund liquidity.
    address public refundCollector;
    bool private _collectorsSet;

    /// @notice The deposit-once tab: funds a payer has deposited but not yet
    /// authorized or charged against a specific payment. Has no counterpart in
    /// the canonical scheme (declared divergence #1/#2/#3 above).
    mapping(address payer => uint256 amount) public balances;

    /// @notice State per unique payment, canonical.
    mapping(bytes32 paymentInfoHash => PaymentState state) public paymentState;

    event Deposited(address indexed payer, uint256 amount);
    event Withdrawn(address indexed payer, uint256 amount);

    event PaymentAuthorized(
        bytes32 indexed paymentInfoHash, PaymentInfo paymentInfo, uint256 amount, address tokenCollector
    );
    event PaymentCharged(
        bytes32 indexed paymentInfoHash,
        PaymentInfo paymentInfo,
        uint256 amount,
        address tokenCollector,
        uint256 feeAmount,
        address feeReceiver
    );
    /// @dev Logs `operator` beyond what canonical emits — v2's Settled event had
    /// no way to tell which operator drove a settlement, an audit-trail gap
    /// flagged during Phase 0. The extra field only extends the event ABI.
    event PaymentCaptured(
        bytes32 indexed paymentInfoHash, address operator, uint256 amount, uint256 feeAmount, address feeReceiver
    );
    event PaymentVoided(bytes32 indexed paymentInfoHash, address operator, uint256 amount);
    event PaymentReclaimed(bytes32 indexed paymentInfoHash, address operator, uint256 amount);
    event PaymentRefunded(bytes32 indexed paymentInfoHash, address operator, uint256 amount, address tokenCollector);

    error InvalidSender(address sender, address expected);
    error ZeroAmount();
    error AmountOverflow(uint256 amount, uint256 limit);
    error ExceedsMaxAmount(uint256 amount, uint256 maxAmount);
    error UnsupportedToken(address token);
    error AfterPreApprovalExpiry(uint48 timestamp, uint48 expiry);
    error InvalidExpiries(uint48 preApproval, uint48 authorization, uint48 refund);
    error FeeBpsOverflow(uint16 feeBps);
    error InvalidFeeBpsRange(uint16 minFeeBps, uint16 maxFeeBps);
    error FeeAmountOutOfRange(uint256 feeAmount, uint256 minFee, uint256 maxFee);
    error ZeroFeeReceiver();
    error InvalidFeeReceiver(address attempted, address expected);
    error InvalidCollectorForOperation();
    error TokenCollectionFailed();
    error PaymentAlreadyCollected(bytes32 paymentInfoHash);
    error AfterAuthorizationExpiry(uint48 timestamp, uint48 expiry);
    error InsufficientAuthorization(bytes32 paymentInfoHash, uint256 authorizedAmount, uint256 requestedAmount);
    error ZeroAuthorization(bytes32 paymentInfoHash);
    error BeforeAuthorizationExpiry(uint48 timestamp, uint48 expiry);
    error AfterRefundExpiry(uint48 timestamp, uint48 expiry);
    error RefundExceedsCapture(uint256 refund, uint256 captured);
    error CollectorsAlreadySet();
    error InsufficientTabBalance(address payer, uint256 available, uint256 requested);
    error ZeroWithdrawAmount();

    modifier onlySender(address sender) {
        if (msg.sender != sender) revert InvalidSender(msg.sender, sender);
        _;
    }

    modifier validAmount(uint256 amount) {
        if (amount == 0) revert ZeroAmount();
        if (amount > type(uint120).max) revert AmountOverflow(amount, type(uint120).max);
        _;
    }

    constructor(address token_) {
        token = IERC20(token_);
        deployer = msg.sender;
    }

    /// @notice Wire up the two trusted collectors. Callable exactly once, by
    /// the deployer only, immediately after both collectors are deployed —
    /// see Deploy.s.sol. Before this runs, paymentCollector/refundCollector are
    /// the zero address, so debitTab and refund's collector check both revert
    /// for everyone; there is no window in which an unwired escrow is usable.
    function setCollectors(address paymentCollector_, address refundCollector_) external onlySender(deployer) {
        if (_collectorsSet) revert CollectorsAlreadySet();
        paymentCollector = paymentCollector_;
        refundCollector = refundCollector_;
        _collectorsSet = true;
    }

    /// @notice Fund the caller's tab. Unchanged from the prior design: credits
    /// what actually arrived, not what was asked for, so a fee-on-transfer
    /// token can never leave the contract owing more than it holds.
    function deposit(uint256 amount) external nonReentrant {
        uint256 balanceBefore = token.balanceOf(address(this));
        token.safeTransferFrom(msg.sender, address(this), amount);
        uint256 received = token.balanceOf(address(this)) - balanceBefore;

        balances[msg.sender] += received;
        emit Deposited(msg.sender, received);
    }

    /// @notice Withdraw up to the caller's tab balance. Partial withdrawal,
    /// unlike the prior all-or-nothing design — a payer no longer has to pull
    /// their entire balance to access part of it.
    function withdraw(uint256 amount) external nonReentrant {
        if (amount == 0) revert ZeroWithdrawAmount();
        if (balances[msg.sender] < amount) revert InsufficientTabBalance(msg.sender, balances[msg.sender], amount);

        balances[msg.sender] -= amount;
        token.safeTransfer(msg.sender, amount);
        emit Withdrawn(msg.sender, amount);
    }

    /// @notice Called only by `paymentCollector`, which has already verified
    /// the payer's signed consent. Debits the tab atomically — this revert is
    /// the substitute for canonical's balance-delta assertion (divergence #2).
    function debitTab(address payer, uint256 amount) external onlySender(paymentCollector) {
        if (balances[payer] < amount) revert InsufficientTabBalance(payer, balances[payer], amount);
        balances[payer] -= amount;
    }

    /// @notice Place a hold on the payer's tab for later capture.
    function authorize(
        PaymentInfo calldata paymentInfo,
        uint256 amount,
        address tokenCollector,
        bytes calldata collectorData
    ) external nonReentrant onlySender(paymentInfo.operator) validAmount(amount) {
        _validatePayment(paymentInfo, amount);

        bytes32 paymentInfoHash = getHash(paymentInfo);
        if (paymentState[paymentInfoHash].hasCollectedPayment) revert PaymentAlreadyCollected(paymentInfoHash);

        paymentState[paymentInfoHash] =
            PaymentState({hasCollectedPayment: true, capturableAmount: uint120(amount), refundableAmount: 0});
        emit PaymentAuthorized(paymentInfoHash, paymentInfo, amount, tokenCollector);

        _collectPayment(paymentInfo, amount, tokenCollector, collectorData);
    }

    /// @notice Autocapture: authorize and pay `receiver` in one call — the
    /// racy control. Two conformant modes of one scheme, not two contracts:
    /// this is what the amplification A/B measures against authorize+capture.
    function charge(
        PaymentInfo calldata paymentInfo,
        uint256 amount,
        address tokenCollector,
        bytes calldata collectorData,
        uint256 feeAmount,
        address feeReceiver
    ) external nonReentrant onlySender(paymentInfo.operator) validAmount(amount) {
        _validatePayment(paymentInfo, amount);
        _validateFee(paymentInfo, amount, feeAmount, feeReceiver);

        bytes32 paymentInfoHash = getHash(paymentInfo);
        if (paymentState[paymentInfoHash].hasCollectedPayment) revert PaymentAlreadyCollected(paymentInfoHash);

        paymentState[paymentInfoHash] =
            PaymentState({hasCollectedPayment: true, capturableAmount: 0, refundableAmount: uint120(amount)});
        emit PaymentCharged(paymentInfoHash, paymentInfo, amount, tokenCollector, feeAmount, feeReceiver);

        _collectPayment(paymentInfo, amount, tokenCollector, collectorData);
        _distributeTokens(paymentInfo.receiver, amount, feeAmount, feeReceiver);
    }

    /// @notice Pay `receiver` out of a previously-authorized hold. Repeatable
    /// up to the cumulative authorized amount, exactly as canonical.
    function capture(PaymentInfo calldata paymentInfo, uint256 amount, uint256 feeAmount, address feeReceiver)
        external
        nonReentrant
        onlySender(paymentInfo.operator)
        validAmount(amount)
    {
        _validateFee(paymentInfo, amount, feeAmount, feeReceiver);

        if (block.timestamp >= paymentInfo.authorizationExpiry) {
            revert AfterAuthorizationExpiry(uint48(block.timestamp), paymentInfo.authorizationExpiry);
        }

        bytes32 paymentInfoHash = getHash(paymentInfo);
        PaymentState memory state = paymentState[paymentInfoHash];
        if (state.capturableAmount < amount) {
            revert InsufficientAuthorization(paymentInfoHash, state.capturableAmount, amount);
        }

        state.capturableAmount -= uint120(amount);
        state.refundableAmount += uint120(amount);
        paymentState[paymentInfoHash] = state;
        emit PaymentCaptured(paymentInfoHash, paymentInfo.operator, amount, feeAmount, feeReceiver);

        _distributeTokens(paymentInfo.receiver, amount, feeAmount, feeReceiver);
    }

    /// @notice Release a hold. Divergence #3: canonical pays the payer's EOA
    /// directly; this re-credits the tab so the funds are available for the
    /// very next prompt.
    function void(PaymentInfo calldata paymentInfo) external nonReentrant onlySender(paymentInfo.operator) {
        bytes32 paymentInfoHash = getHash(paymentInfo);
        uint256 authorizedAmount = paymentState[paymentInfoHash].capturableAmount;
        if (authorizedAmount == 0) revert ZeroAuthorization(paymentInfoHash);

        paymentState[paymentInfoHash].capturableAmount = 0;
        emit PaymentVoided(paymentInfoHash, paymentInfo.operator, authorizedAmount);

        balances[paymentInfo.payer] += authorizedAmount;
    }

    /// @notice Payer self-serve release after the operator lets the hold
    /// expire without capturing. Same tab re-credit as void().
    function reclaim(PaymentInfo calldata paymentInfo) external nonReentrant onlySender(paymentInfo.payer) {
        if (block.timestamp < paymentInfo.authorizationExpiry) {
            revert BeforeAuthorizationExpiry(uint48(block.timestamp), paymentInfo.authorizationExpiry);
        }

        bytes32 paymentInfoHash = getHash(paymentInfo);
        uint256 authorizedAmount = paymentState[paymentInfoHash].capturableAmount;
        if (authorizedAmount == 0) revert ZeroAuthorization(paymentInfoHash);

        paymentState[paymentInfoHash].capturableAmount = 0;
        emit PaymentReclaimed(paymentInfoHash, paymentInfo.operator, authorizedAmount);

        balances[paymentInfo.payer] += authorizedAmount;
    }

    /// @notice Return previously-captured funds to the payer. No divergence:
    /// capture() already paid `receiver` out, so this needs fresh liquidity —
    /// pulled from the operator's own allowance via `refundCollector`, exactly
    /// as canonical's OperatorRefundCollector does. The balance-delta check
    /// applies unmodified here because a real transfer really does happen.
    function refund(PaymentInfo calldata paymentInfo, uint256 amount, address tokenCollector, bytes calldata collectorData)
        external
        nonReentrant
        onlySender(paymentInfo.operator)
        validAmount(amount)
    {
        if (block.timestamp >= paymentInfo.refundExpiry) {
            revert AfterRefundExpiry(uint48(block.timestamp), paymentInfo.refundExpiry);
        }

        bytes32 paymentInfoHash = getHash(paymentInfo);
        uint120 captured = paymentState[paymentInfoHash].refundableAmount;
        if (captured < amount) revert RefundExceedsCapture(amount, captured);

        paymentState[paymentInfoHash].refundableAmount = captured - uint120(amount);
        emit PaymentRefunded(paymentInfoHash, paymentInfo.operator, amount, tokenCollector);

        if (ITokenCollector(tokenCollector).collectorType() != ITokenCollector.CollectorType.Refund) {
            revert InvalidCollectorForOperation();
        }
        uint256 balanceBefore = token.balanceOf(address(this));
        ITokenCollector(tokenCollector).collectTokens(paymentInfo, address(this), amount, collectorData);
        if (token.balanceOf(address(this)) != balanceBefore + amount) revert TokenCollectionFailed();

        token.safeTransfer(paymentInfo.payer, amount);
    }

    /// @notice Canonical hash derivation: includes chainId and this contract's
    /// address so a signature over one payment can never settle on another
    /// escrow instance or another chain.
    function getHash(PaymentInfo calldata paymentInfo) public view returns (bytes32) {
        bytes32 paymentInfoHash = keccak256(abi.encode(PAYMENT_INFO_TYPEHASH, paymentInfo));
        return keccak256(abi.encode(block.chainid, address(this), paymentInfoHash));
    }

    /// @dev Collection for authorize()/charge(). See divergence #2: no
    /// balance-delta assertion, because deposit-once has no external transfer
    /// to observe here. debitTab() is the atomic substitute.
    function _collectPayment(
        PaymentInfo calldata paymentInfo,
        uint256 amount,
        address tokenCollector,
        bytes calldata collectorData
    ) internal {
        if (ITokenCollector(tokenCollector).collectorType() != ITokenCollector.CollectorType.Payment) {
            revert InvalidCollectorForOperation();
        }
        ITokenCollector(tokenCollector).collectTokens(paymentInfo, address(this), amount, collectorData);
    }

    function _distributeTokens(address receiver, uint256 amount, uint256 feeAmount, address feeReceiver) internal {
        if (feeAmount > 0) token.safeTransfer(feeReceiver, feeAmount);
        if (amount > feeAmount) token.safeTransfer(receiver, amount - feeAmount);
    }

    function _validatePayment(PaymentInfo calldata paymentInfo, uint256 amount) internal view {
        if (paymentInfo.token != address(token)) revert UnsupportedToken(paymentInfo.token);

        uint120 maxAmount = paymentInfo.maxAmount;
        uint48 preApprovalExp = paymentInfo.preApprovalExpiry;
        uint48 authorizationExp = paymentInfo.authorizationExpiry;
        uint48 refundExp = paymentInfo.refundExpiry;
        uint16 minFeeBps = paymentInfo.minFeeBps;
        uint16 maxFeeBps = paymentInfo.maxFeeBps;
        uint48 currentTime = uint48(block.timestamp);

        if (amount > maxAmount) revert ExceedsMaxAmount(amount, maxAmount);
        if (currentTime >= preApprovalExp) revert AfterPreApprovalExpiry(currentTime, preApprovalExp);
        if (preApprovalExp > authorizationExp || authorizationExp > refundExp) {
            revert InvalidExpiries(preApprovalExp, authorizationExp, refundExp);
        }
        if (maxFeeBps > _MAX_FEE_BPS) revert FeeBpsOverflow(maxFeeBps);
        if (minFeeBps > maxFeeBps) revert InvalidFeeBpsRange(minFeeBps, maxFeeBps);
    }

    function _validateFee(PaymentInfo calldata paymentInfo, uint256 amount, uint256 feeAmount, address feeReceiver)
        internal
        pure
    {
        address configuredFeeReceiver = paymentInfo.feeReceiver;
        uint256 minFee = amount * paymentInfo.minFeeBps / _MAX_FEE_BPS;
        uint256 maxFee = amount * paymentInfo.maxFeeBps / _MAX_FEE_BPS;

        if (feeAmount < minFee || feeAmount > maxFee) revert FeeAmountOutOfRange(feeAmount, minFee, maxFee);
        if (feeReceiver == address(0) && feeAmount > 0) revert ZeroFeeReceiver();
        if (configuredFeeReceiver != address(0) && configuredFeeReceiver != feeReceiver) {
            revert InvalidFeeReceiver(feeReceiver, configuredFeeReceiver);
        }
    }
}
