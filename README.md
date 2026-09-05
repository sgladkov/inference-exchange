# Inference Exchange

A broker for agent-to-agent inference: per-call x402 payments on Hedera, with spend limits.

One agent delegates a task to another and pays for the result in HBAR. Providers dial in and never
listen on a port, so a laptop behind NAT can sell. Every payment passes a spend policy that the
buying agent cannot route around.

```
0.0.7162784@1788610655.578564070   SUCCESS on Hedera testnet
    0.0.7162784   -248996   ← Blocky402 pays the network fee
   0.0.10123224        -6   ← buyer
   0.0.10236261        +6   ← provider, paid directly
```

## Why payment comes last

The x402 exact scheme normally wraps the work: verify the payment, do the job, settle. That caps
job duration at Hedera's **[180-second transaction validity window](https://docs.hedera.com/hedera/core-concepts/transactions-and-queries)**
— the signed transfer expires while the work is still running, and the provider does the job for
nothing.

So payment is deferred instead. Dispatch is free and returns a job id immediately, while the work
runs in the background; the buyer polls until it completes and pays at collect, for actual usage,
bounded by a ceiling it declared up front. No transaction is in flight while the work runs.

**Verified:** a 200-second job — past the window — dispatches, completes and settles. The payment
round trip is about two seconds at the very end.

What that buys, and what it costs:

- Job duration is unbounded. Real agentic work takes minutes.
- Pricing is on **actual** usage rather than an estimate, still clamped to the buyer's ceiling.
- A failed job is never billable — the error comes back free. That replaces the delivery-conditional
  guarantee lost by moving payment out from between verify and settle.
- Free dispatch is an abuse surface, so an abandonment ratio bounds how many jobs a buyer may
  commission and never collect.

## Architecture

```
buyer: agent + client (JS)              seller: provider daemon (JS)
   │ delegate()                                │ dials out (WS), never listens
   ▼                                           ▼
┌──────────────────────────────────────────────────────────┐
│  REGISTRY / BROKER  (Go)  — the only public surface       │
│    x402 resource server · job store · dispatcher · policy │
└──────────────────────┬───────────────────────────────────┘
                       │ /verify /settle  (plain JSON HTTP)
                       ▼
              BLOCKY402 (third party) ──▶ Hedera testnet
```

**The registry is the only x402 resource server**, which is what makes its spend policy enforcing
rather than advisory. A control shipped inside the buying agent would be a suggestion: the agent
holds the key and could construct the same payment without it. Here there is nowhere else to pay.

**The registry never holds funds.** Payment settles buyer → provider directly. It sets the payee
from the provider's own registration, so it is a broker, not a custodian.

**The registry never opens the signed transaction.** It authors the payment requirements from its
own job record and hands them to the facilitator, which checks the transfer against exactly those.
That is why the Go side needs no Hedera SDK on the payment path.

**Providers hold no private key.** Payments land at the account declared at registration; there is
nothing to withdraw and nothing to sign with.

## Payment flow

```
POST /p/{provider}/job          free · 202 with a job id, immediately
  └─ registry → provider over the open socket
     provider executes, reports units used
     registry clamps units to the buyer's ceiling and prices them

GET  /p/job/{id}/status         free · running → completed, with the price

GET  /p/job/{id}                402 + PAYMENT-REQUIRED
  └─ buyer signs a transfer naming the facilitator as fee payer
GET  /p/job/{id}                PAYMENT-SIGNATURE → verify → settle → 200 + result
```

Dispatch returns before the work does, so the buyer holds the job id from the outset. That is what
lets it report real progress rather than a heartbeat, and what lets it reclaim a job after a crash
— the result is held for collection either way.

Between the challenge and the result the registry checks, in order:

| Guard | Catches |
| --- | --- |
| `accepted-mismatch` | a challenge captured against a different job, replayed against this one |
| facilitator `verify` | a signed transfer that disagrees with the stated requirements |
| `payer-mismatch` | someone paying for a job they did not commission |
| spend policy | the real price against caps, budget, velocity, abandonment |

An unreachable facilitator **denies** — it never passes through unsettled, and its class is
distinct from a policy denial so the caller retries rather than changing its budget.

## Setup

Requires Go 1.26+, Node 24+, pnpm, and a funded Hedera testnet account.

```bash
pnpm install
cd registry && go build -o ../bin/registry ./cmd/registry && cd ..

export MERCHANT_ID=0.0.xxxxx        # buyer account
export MERCHANT_KEY=0x...           # its ECDSA key
export SELLER_ID=0.0.yyyyy          # provider payee account

./bin/registry -addr :8080 &

node packages/provider/daemon.mjs \
  --registry http://localhost:8080 --account "$SELLER_ID" \
  --backend echo --rate 3 &

node demo/e2e.mjs --registry http://localhost:8080
```

The registry discovers the facilitator's fee payer at startup and exits if it cannot reach it —
without that value it would issue challenges nobody can pay.

### Provider backends

`--backend` selects what the provider actually sells. Registration, wire protocol and payment path
are identical regardless.

| Backend | Runs |
| --- | --- |
| `echo` | Deterministic, free. `sleep:<n>` as a prompt takes that many seconds, for demonstrating long jobs |
| `claude-code` | A local Claude Code instance, headless. One agent paying another |
| `anthropic` | The Messages API, with the provider's own key |
| `ollama` | A local model |

## Tests

```bash
cd registry && go test ./... -race     # 100+ tests, no network
pnpm -r test                            # provider and client
node demo/e2e.mjs --registry ...        # live testnet, settles real HBAR
E2E_LONG_JOB=1 node demo/e2e.mjs ...    # adds the 200-second job, ~3.5 min
```

The Go x402 implementation is tested against `fixtures/` — wire bytes captured from a real
settlement with `fetch` intercepted, so the tests assert against what the reference implementation
actually emits rather than against documentation. The challenge round-trip is byte-for-byte.

`demo/e2e.mjs` runs every denial before its matching success, so a pass means the control fired
rather than that the happy path happened to work.

## Known limits

- **Buyer identity is asserted at dispatch, not authenticated.** It is checked where it matters:
  the facilitator reports who actually signed, and a mismatch is refused. So a caller can attribute
  a *free* job to another account, but cannot spend their money or read their results.
- **No per-provider concurrency limit.** Synchronous dispatch used to provide incidental
  backpressure — a buyer awaiting a response was not issuing more. Asynchronous dispatch removes
  that, and only the per-buyer velocity rule bounds the rate. A provider serving several buyers at
  once can be given more concurrent work than it can handle.
- **Waiting is client-side.** `waitFor` polls; there is no push. A caller that gives up gets
  `StillRunningError` rather than a failure, and can return later with the job id, but nothing
  notifies it.
- **State is in memory.** Nothing is custodial, so a restart costs at most the jobs in flight.
- **Testnet only.** The facilitator advertises no `hedera:mainnet` kind.
- **No marketplace fee.** The exact scheme rejects a third credited party
  (`invalid_exact_hedera_payload_extra_positive_transfers`), so a fee would have to be out of band.
```
