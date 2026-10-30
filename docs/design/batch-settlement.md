# `batch-settlement`: a contract that takes the balance read off the request path

Status: follow-up, nothing scheduled. It is picked up only if the core path's fallbacks do
not hold (see "When to pick it up"). Contracts, facilitators and opcodes were checked live
on 2026-10-29; latencies are from [request-path-budget.md](request-path-budget.md).

## Summary

The core path has one slow step: on a cache miss the filter reads the payer's balance over
RPC, and that read has an unbounded tail (p99 123 ms warm, spikes to 2 s, every new
connection past 140 ms). Faster settings do not fix it, because a wallet balance can drop
at any moment when the payer spends elsewhere, so any cached value is a guess.

x402's `batch-settlement` scheme changes what the filter has to know. The buyer deposits
once into an escrow contract and then pays each request with a signed, cumulative voucher.
Escrowed funds cannot leave without a withdrawal delay of at least 15 minutes, so the
channel's balance is safe to cache, and a voucher can be checked locally: a signature
recovery and a Redis compare, with no RPC call per request.

For this gateway: the contracts are **not deployed on Radius**, though the EVM features
they need work there; **no facilitator on Radius and no Radius SDK supports the scheme**;
and it suits repeat buyers rather than one-off calls. Deploying the contracts is within
reach (this project's author has deployed contracts to Radius testnet before), so it is a
credible follow-up, not a blocker.

## The problem it addresses

From the in-cluster baseline (2026-10-28, [request-path-budget.md](request-path-budget.md)):

- A cache hit costs about 5 ms at p99. A cache miss on a warm RPC connection costs about
  130 ms at p99 and fits the filter's 150 ms deadline 99.26% of the time.
- The other 0.74% includes spikes up to 2 s. A cold connection never fits.
- A payer's first request is always a miss.

The core-path answer is to keep the RPC connection warm, own the deadline in the filter,
and fall back (let the timed-out read fill the cache; serve on the last known balance)
instead of returning an error. That bounds the problem but keeps the RPC on the request
path. `batch-settlement` removes it.

## How the scheme works

From the EVM binding (v1.0, marked as an initial draft).

**A channel.** A `ChannelConfig` names `payer`, `payerAuthorizer`, `receiver`,
`receiverAuthorizer`, `token`, `withdrawDelay` and `salt`. The channel id is the EIP-712
hash of that struct under the `x402 Batch Settlement` domain. A channel is created by its
first deposit.

**The first request deposits.** When no channel exists, or its balance is used up, the
client sends a `deposit` payload: the `channelConfig`, a token authorization (EIP-3009 or
Permit2, exactly one), and the first voucher. The facilitator submits the deposit and
sponsors its gas.

**Later requests carry a voucher.** A voucher names the `channelId` and a
`maxClaimableAmount`, the running total the payer has agreed to. The server checks that
`maxClaimableAmount == chargedCumulativeAmount + amount`. The spec allows local checking:
"A plain EOA-authorized `voucher` may be verified locally when the server's mirrored
onchain state is fresh." It does not define "fresh". Vouchers from smart-contract wallets
(EIP-1271) need the facilitator's `/verify`.

**Settling a request is a state update.** The server commits the new cumulative amount
only after the resource handler succeeds, and requests on one channel are serialized.
Money moves later: `claim` (or `claimWithSignature`, signed by the `receiverAuthorizer`)
records claimed amounts across many channels, and a permissionless `settle` moves all
claimed funds to the receiver in one transfer.

**Leaving.** The receiver side can refund unclaimed funds at once. The payer can
`initiateWithdraw` and, after `withdrawDelay` (15 minutes to 30 days), `finalizeWithdraw`.
The spec is explicit about the risk: "The server must claim all outstanding vouchers before
the withdraw delay elapses," and vouchers left unclaimed after `finalizeWithdraw` are lost.

**Server state per channel.** `channelConfig`, `chargedCumulativeAmount`,
`signedMaxClaimable`, the latest `signature`, mirrored `balance`, `totalClaimed`,
`withdrawRequestedAt`, `refundNonce`, `onchainSyncedAt`, `lastRequestTimestamp`. The
mirrored fields are refreshed from facilitator responses.

## Why the balance becomes safe to cache

| | `exact` (core path) | `batch-settlement` |
|---|---|---|
| What the filter must know | The payer's wallet balance now | The channel's balance minus what is already claimed or charged |
| Can it drop without warning? | Yes: the payer can spend anywhere, any time | Only through `initiateWithdraw`, which is on chain and starts a delay of ≥ 15 min |
| Per-request check | Signature + Redis + `balanceOf` on a miss | Signature + Redis compare-and-set |
| RPC on the request path | On every cache miss | On the deposit only |
| On-chain transactions | One transfer per paid request (~$0.0001 each) | One deposit, then periodic `claim` + `settle` |

The second row also closes a risk the core path accepts: a buyer who pays several sellers
from one wallet can be served by all of them on a cached balance and leave some unpaid.
Under a channel, the funds are already committed to this receiver.

## Checked on 2026-10-29

Read-only `eth_getCode`, `eth_call` and `GET /supported`. Byte counts are deployed code size.

| What | Radius testnet `eip155:72344` | Radius mainnet `eip155:723487` | Base Sepolia `eip155:84532` |
|---|---|---|---|
| `x402BatchSettlement` `0x4020074e9dF2ce1deE5A9C1b5c3f541D02a10003` | none | none | 11175 bytes |
| `ERC3009DepositCollector` `0x4020806089470a89826cB9fB1f4059150b550004` | none | none | not checked |
| `Permit2DepositCollector` `0x4020425FAf3B746C082C2f942b4E5159887B0005` | none | none | not checked |
| EIP-1153 transient storage (the contract uses it) | works | works | works |
| `upto` proxy `0x4020A4f3b7b90ccA423B9fabCc0CE57C6C240002`, for comparison | 3142 bytes | 3142 bytes | — |

The opcode check ran initcode that stores 42 with `TSTORE`, reads it back with `TLOAD` and
returns it; each network returned 42.

- **Facilitators.** Both Radius facilitators advertise only `exact`. `x402.org/facilitator`
  advertises `batch-settlement` on Base Sepolia only.
- **radius-sdk.** x402 `exact` and `upto`, and MPP `charge`. No channels or sessions.
- **MPP sessions** use the same deposit-and-voucher idea, but as Tempo's payment method,
  which radius-sdk does not emit.
- **Radius RPC** has no `eth_subscribe` or `eth_newFilter`, and `eth_getLogs` requires an
  `address`. Channel state can be polled, not pushed.

## How it would map onto this gateway

- **Request phase, voucher.** Recover the signer, load the channel from Redis, check
  `maxClaimableAmount == charged + price` and that it stays within the mirrored balance
  minus `totalClaimed`, and refuse if a withdrawal is pending and too close to finishing.
  A Lua script or `WATCH` makes the compare-and-set atomic per channel, which is the spec's
  serialization rule.
- **Request phase, deposit.** The deposit is an on-chain transaction through the
  facilitator. That is the measured `/settle` cost (0.60–1.13 s from a laptop), which only
  fits the Sync route shape and its 2 s Envoy timeout, not the 150 ms Async deadline. The
  first request of a channel is slow by design; every later one is local.
- **Response phase.** Commit the new cumulative amount on a 2xx and drop it otherwise. This
  is the same settle-after rule the core path uses.
- **Settler.** Instead of one `/settle` per request, periodic `claim` + `settle` per
  receiver, triggered by time, by an unclaimed-amount threshold, or by a channel entering
  its withdrawal window.
- **Reconciler.** Poll channel state for channels with a pending withdrawal, because there
  is no push.

## What it costs

- **A key or a delegation.** `claim` must come from the `receiver` or `receiverAuthorizer`,
  and `claimWithSignature` needs the `receiverAuthorizer`'s EIP-712 signature (it must not
  be the zero address). Either the cluster holds a key, ending the "no private keys"
  property, or a facilitator signs on our behalf and authenticates us. This is the same
  trade-off as `auth-capture`.
- **Durable state that is money.** Losing the Redis record of a voucher before it is
  claimed loses revenue. Redis persistence, or claiming often, becomes a requirement.
- **A claim deadline.** A missed claim before `finalizeWithdraw` is lost. The settler has to
  watch withdrawal windows.
- **One request at a time per channel.** Parallel requests on one channel conflict, because
  each voucher builds on the last cumulative amount. An agent making parallel calls needs
  several channels (different `salt`) or queues on our side.
- **A buyer that supports it.** The x402 TypeScript and Go SDKs do; radius-sdk does not.
  One-off buyers will not deposit; `exact` stays for them.
- **A `scheme` per route,** as for `auth-capture`. The price table and `PaymentPolicy` carry
  none today.

## When to pick it up

Trigger, measured after M5 (warm RPC connection, filter deadline, fallbacks in place): the
share of paid requests that hit the deadline or the fallback stays above a budget the
seller accepts, or the keyed RPC under load shows a tail or rate limits the fallbacks
cannot absorb. The budget itself is open.

Preconditions:

1. The three contracts deployed on Radius testnet. Open: whether they can be deployed at the
   canonical addresses (the spec names CREATE2 but not the factory, salt or deployer). A
   deployment at other addresses works only for clients and facilitators configured with
   them.
2. A facilitator on Radius that supports the scheme (Radius adding it, or the reference Go
   facilitator run by us), or the gateway submitting deposits and claims itself.
3. Buyers who make repeat calls, so a deposit pays for itself.

## Sources

- [`batch-settlement` scheme](https://github.com/coinbase/x402/blob/main/specs/schemes/batch-settlement/scheme_batch_settlement.md) and its [EVM binding](https://github.com/coinbase/x402/blob/main/specs/schemes/batch-settlement/scheme_batch_settlement_evm.md), v1.0
- [x402 docs: batch settlement](https://docs.x402.org/schemes/batch-settlement)
- [radius-sdk reference](https://docs.radiustech.xyz/reference/radius-sdk.md): supported schemes and MPP intents
- [Radius JSON-RPC methods](https://docs.radiustech.xyz/reference/json-rpc/methods.md): unsupported subscription and filter methods
- [auth-capture.md](auth-capture.md): the related escrow scheme, and the key trade-off
- [request-path-budget.md](request-path-budget.md): the latency measurements quoted above
