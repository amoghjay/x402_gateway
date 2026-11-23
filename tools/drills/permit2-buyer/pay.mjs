// Pay an x402 route with the reference client (@x402/evm), choosing the Permit2 offer.
// usage: node pay.mjs <keyfile> <url> [--approve] [--status] [--sign-only]
//   --status     print balances and the Permit2 allowance, pay nothing
//   --approve    send the one-time approve(Permit2, max) if the allowance is short (costs ETH)
//   --sign-only  print the PAYMENT-SIGNATURE header for the Permit2 offer, do not send it
import { readFileSync } from "node:fs";
import { createPublicClient, createWalletClient, http, formatUnits, formatEther, erc20Abi } from "viem";
import { privateKeyToAccount } from "viem/accounts";
import { baseSepolia } from "viem/chains";
import { x402Client, x402HTTPClient, wrapFetchWithPayment, decodePaymentResponseHeader } from "@x402/fetch";
import { registerExactEvmScheme } from "@x402/evm/exact/client";
import { createPermit2ApprovalTx, getPermit2AllowanceReadParams } from "@x402/evm/exact/client";

const [keyfile, url, ...flags] = process.argv.slice(2);
if (!keyfile || !url) { console.error("usage: node pay.mjs <keyfile> <url> [--approve|--status|--sign-only]"); process.exit(2); }
let key = readFileSync(keyfile, "utf8").trim();
if (!key.startsWith("0x")) key = "0x" + key;
const account = privateKeyToAccount(key);
const rpc = http(process.env.BASE_SEPOLIA_RPC || "https://sepolia.base.org");
const pub = createPublicClient({ chain: baseSepolia, transport: rpc });

// Find the Permit2 offer in the route's 402.
const first = await fetch(url);
if (first.status !== 402) { console.error(`expected 402, got ${first.status}`); process.exit(1); }
const req = JSON.parse(Buffer.from(first.headers.get("payment-required"), "base64").toString());
const offer = req.accepts.find((a) => a.extra?.assetTransferMethod === "permit2");
if (!offer) { console.error("no permit2 offer in the 402"); process.exit(1); }
const token = offer.asset;
const [bal, eth, allowance] = await Promise.all([
  pub.readContract({ address: token, abi: erc20Abi, functionName: "balanceOf", args: [account.address] }),
  pub.getBalance({ address: account.address }),
  pub.readContract(getPermit2AllowanceReadParams({ tokenAddress: token, ownerAddress: account.address })),
]);
const dec = offer.extra?.decimals ?? 6;
console.log(`buyer ${account.address}`);
console.log(`offer ${offer.network} ${token} amount ${offer.amount} payTo ${offer.payTo} facilitator ${offer.facilitator ?? "(none in offer)"}`);
console.log(`token balance ${formatUnits(bal, dec)}  ETH ${formatEther(eth)}  Permit2 allowance ${allowance}`);
if (flags.includes("--status")) process.exit(0);

if (allowance < BigInt(offer.amount)) {
  if (!flags.includes("--approve")) { console.error("Permit2 allowance too small: rerun with --approve (sends one tx, costs ETH)"); process.exit(1); }
  const wallet = createWalletClient({ account, chain: baseSepolia, transport: rpc });
  const tx = createPermit2ApprovalTx(token);
  const hash = await wallet.sendTransaction({ to: tx.to, data: tx.data });
  const r = await pub.waitForTransactionReceipt({ hash });
  console.log(`approve tx ${hash} status ${r.status}`);
  if (r.status !== "success") process.exit(1);
}

const client = new x402Client((_v, reqs) => reqs.find((r) => r.extra?.assetTransferMethod === "permit2"));
registerExactEvmScheme(client, { signer: account });
// The client pays only its built-in default assets unless told otherwise; allow exactly
// this offer's token, capped at its price.
client.setSpendControls({ allowedAssets: [{ network: offer.network, asset: token, maxAmountPerPayment: offer.amount }] });

if (flags.includes("--sign-only")) {
  const http402 = new x402HTTPClient(client);
  const payload = await client.createPaymentPayload(req);
  const headers = http402.encodePaymentSignatureHeader(payload);
  console.log(Object.values(headers)[0]);
  process.exit(0);
}

const t0 = Date.now();
const res = await wrapFetchWithPayment(fetch, client)(url);
console.log(`HTTP ${res.status} in ${Date.now() - t0} ms`);
const pr = res.headers.get("payment-response");
if (pr) console.log("payment-response", JSON.stringify(decodePaymentResponseHeader(pr)));
else console.log("body", (await res.text()).slice(0, 300));
