// SPDX-License-Identifier: MIT
pragma solidity ^0.8.20;

import {Script} from "forge-std/Script.sol";
import {console} from "forge-std/console.sol";

import {InferenceEscrow} from "../src/InferenceEscrow.sol";

/// @notice Deploys InferenceEscrow.
///
/// The live deployment at 0x76c03C8b763a0e2f3594bABfb71847e8a6d502D8 was created
/// by hand, so its constructor arguments existed only in shell history. This
/// script makes the deployment reproducible; its arguments were checked against
/// the live contract's `token()` and `provider()` getters before being written.
///
/// `provider` is immutable, so it cannot be changed after deploy — pointing
/// payment somewhere else means a new contract, which is why PAY_TO_ADDRESS is
/// read here rather than passed per settlement.
///
/// No private key is read from the environment: the signer comes from the
/// command line, so a key never has to live in a file this script can see.
///
///   forge script script/Deploy.s.sol \
///     --rpc-url "$RPC_URL" \
///     --private-key "$GATEWAY_OPERATOR_KEY" \
///     --broadcast
///
/// Afterwards, propagate the new address to ESCROW_CONTRACT_ADDRESS in .env and
/// .env.example, then re-run contracts/sync-abi.sh if the ABI changed.
contract Deploy is Script {
    function run() external returns (InferenceEscrow escrow) {
        address token = vm.envAddress("SBC_CONTRACT_ADDRESS");
        address provider = vm.envAddress("PAY_TO_ADDRESS");

        vm.startBroadcast();
        escrow = new InferenceEscrow(token, provider);
        vm.stopBroadcast();

        console.log("InferenceEscrow deployed:", address(escrow));
        console.log("  token   :", token);
        console.log("  provider:", provider);
    }
}
