// SPDX-License-Identifier: MIT
pragma solidity ^0.8.20;

import {Script} from "forge-std/Script.sol";
import {console} from "forge-std/console.sol";

import {InferenceEscrow} from "../src/InferenceEscrow.sol";
import {InferenceEscrowCollector} from "../src/InferenceEscrowCollector.sol";
import {OperatorRefundCollector} from "../src/OperatorRefundCollector.sol";

/// @notice Deploys the v3 auth-capture InferenceEscrow plus its two trusted
/// collectors, then wires them together.
///
/// The v2 deployment at 0x76c03C8b763a0e2f3594bABfb71847e8a6d502D8 pinned
/// `provider` as an immutable constructor argument. In v3 the receiver is a
/// PaymentInfo field instead (per the auth-capture scheme), supplied per
/// payment by the gateway rather than baked into the contract — PAY_TO_ADDRESS
/// now belongs to the gateway's config, not this script.
///
/// No private key is read from the environment: the signer comes from the
/// command line, so a key never has to live in a file this script can see.
///
///   forge script script/Deploy.s.sol \
///     --rpc-url "$RPC_URL" \
///     --private-key "$GATEWAY_OPERATOR_KEY" \
///     --broadcast
///
/// Afterwards, propagate the new addresses to .env / .env.example, then
/// re-run contracts/sync-abi.sh if the ABI changed.
contract Deploy is Script {
    function run()
        external
        returns (InferenceEscrow escrow, InferenceEscrowCollector paymentCollector, OperatorRefundCollector refundCollector)
    {
        address token = vm.envAddress("SBC_CONTRACT_ADDRESS");

        vm.startBroadcast();
        escrow = new InferenceEscrow(token);
        paymentCollector = new InferenceEscrowCollector(address(escrow));
        refundCollector = new OperatorRefundCollector(address(escrow));
        escrow.setCollectors(address(paymentCollector), address(refundCollector));
        vm.stopBroadcast();

        console.log("InferenceEscrow deployed      :", address(escrow));
        console.log("InferenceEscrowCollector      :", address(paymentCollector));
        console.log("OperatorRefundCollector       :", address(refundCollector));
        console.log("  token                       :", token);
    }
}
