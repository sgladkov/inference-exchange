# Architecture

The README covers what the exchange does and how to run it. This covers how it is built, where the
trust boundaries sit, and why the three decisions that shaped it were made the way they were.

## Components

```
  BUYER SIDE                         BROKER                        SELLER SIDE
  ──────────                         ──────                        ───────────
  host agent (Claude Code)                                  provider daemon (Node)
    └ MCP server ────┐                                        ├ backend: claude-code
        6 tools      │                                        ├ backend: anthropic
                     ▼                                        ├ backend: ollama
      buyer client (Node) ──── HTTPS ──▶ ┌──────────────┐     └ backend: echo
        · signs payments                 │  REGISTRY    │              ▲
        · holds the only private key     │    (Go)      │              │
                                         │              │◀── WebSocket ┘
                                         │ x402 server  │    (provider dials OUT)
                                         │ job store    │
                                         │ dispatcher   │
                                         │ spend policy │
                                         │ HCS audit    │
                                         └──────┬───────┘
                                                │ /verify /settle
                                                ▼
                                    BLOCKY402 facilitator
                                                │
                                                ▼
                                         Hedera testnet
```

| Component | Language | Responsibility |
| --- | --- | --- |
| `registry/` | Go | The broker. Only publicly reachable component. x402 resource server, job store, dispatcher, spend policy, HCS audit log |
| `packages/client/` | Node | Buyer side. Discovers providers, quotes, dispatches, signs and submits payment |
| `packages/mcp/` | Node | Installable MCP surface — six tools an agent calls |
| `packages/provider/` | Node | Seller side. Dials out, quotes prompts, runs a backend |
| `demo/` | Node | `showcase.mjs` (the demo), `e2e.mjs` (21 live checks), `rigged-provider.mjs` (a provider that misbehaves on cue) |

### Inside the registry

| Package | Holds |
| --- | --- |
| `cmd/registry` | HTTP handlers: `quote.go`, `dispatch.go`, `collect.go`, `spend.go`, wiring in `main.go` |
| `internal/hub` | Provider WebSocket connections; the quote and job frame protocol |
| `internal/store` | Providers, quotes and jobs. In memory, `sync.Mutex`-guarded |
| `internal/policy` | Spend rules. Pure functions over a `Request`, no I/O |
| `internal/x402` | Facilitator client and wire types, tested against captured bytes |
| `internal/hcs` | The decision log writer |

130 Go tests, plus three Node suites and a live end-to-end demo that settles real HBAR.

## Trust boundaries

This is the part worth reading twice, because nearly every design decision falls out of it.

| Boundary | What is assumed | Consequence |
| --- | --- | --- |
| **Host agent → buyer client** | The agent may be compromised. Its task comes from its inputs, and its inputs may be attacker-controlled | The client holds **no policy that matters**. Budgets and caps live in the registry, which is the only x402 resource server, so there is nowhere else to pay |
| **Buyer → registry** | Buyer identity is *asserted*, not authenticated | A caller can attribute a **free** job to another account, but cannot spend their money: the facilitator reports who actually signed, and a mismatch is refused |
| **Registry → provider** | The provider self-reports its model, backend, and token usage. **None of it is verified** | Everything a provider claims sits under `declared` in the API, never beside settled facts. Usage is clamped to the quote it gave |
| **Registry → facilitator** | Trusted to verify and settle honestly | Neither protocol-enforced nor client-verifiable. Stated plainly rather than hidden |
| **Anyone → decision log** | The registry could lie about what it decided | The log is on a public HCS topic, read from a **mirror node**. A buyer auditing the exchange never has to ask the exchange |

**The registry never holds funds and never opens the signed transaction.** It authors payment
requirements from its own job record and hands them to the facilitator, which checks the transfer
against exactly those. Payment settles buyer → provider directly. That is why the Go side needs no
Hedera SDK on the payment path — and why a compromised registry can refuse service but cannot steal.

**Providers hold no private key.** Payments land at the account named at registration. There is
nothing to withdraw and nothing to sign with.

## Three decisions, and why

### 1. The registry is a broker, not a directory

Providers **dial out** over a WebSocket and never listen on a port. A laptop behind NAT can sell.
The registry is the only publicly reachable component, which also makes it the only place a spend
policy can be enforced rather than suggested.

The cost is a single point of failure: if the registry is down, the exchange is down. Accepted,
because the alternative — policy inside the buying agent — is not a control at all. The agent holds
the key and could construct the same payment without it.

### 2. Payment is deferred

The x402 exact scheme normally wraps the work: verify, do the job, settle. That caps job duration at
Hedera's 180-second transaction validity window — the signed transfer expires while the work is
still running, and the provider works for nothing.

So dispatch is free and returns a job id immediately; the buyer polls and pays at collect. **No
transaction is in flight while the work runs.** Verified with a 200-second job that settles.

What it costs, and how each cost is answered:

| Cost | Answer |
| --- | --- |
| Delivery is no longer guaranteed by the protocol | A failed job is never billable — the error comes back free |
| Free dispatch is an abuse surface | An abandonment ratio bounds jobs commissioned and never collected |
| The result must be held somewhere | `result-ttl`, after which it expires and nobody is charged |

### 3. The price comes from the provider

A buyer cannot know what a prompt will cost — usage depends on the prompt *and* the backend, and
only the provider can judge the pair. The buyer used to declare `max_units` as a blind ceiling, and
`Complete` clamped to it **with no floor**, so `max_units: 1` bought a full `claude-code` job for
three tinybar.

Now the provider prices the specific prompt, and the quote is **binding on it**: overrun comes out
of its margin. Either party may walk — the provider can decline with a reason, and a decline is a
first-class outcome that is never scored as a failure.

**This relocates the metering-trust problem rather than solving it.** Over-reporting past the quote
gains nothing, so the incentive moves into the estimate: pad it, then report up to it. The clamp
cannot see that, because the provider supplies both numbers. The checks are **competition** — which
is why quoting is a separate call a buyer can make to several providers with one prompt — and
observed quote accuracy. Neither is a proof.

## Job lifecycle

```
   POST /p/{id}/quote  ──▶  provider prices this prompt  ──▶  quote (5 min TTL, single use)
                                    │
                                    └─ or declines, with a reason  → 409
   POST /p/{id}/job {quote_id}  ──▶  running        202, free, returns immediately
                                    │                 the prompt comes from the quote and is
                                    │                 never resent, so it cannot be swapped
                            ┌───────┴────────┐
                         completed         failed  ──▶  error returned free, never billable
                            │
   GET /p/job/{id}     ──▶  402 + PAYMENT-REQUIRED
                            │  buyer signs a transfer naming the facilitator as fee payer
                       ──▶  PAYMENT-SIGNATURE → verify → settle → 200 + result
                            │
                         collected                 (or expired, past result-ttl)
```

Guards between the challenge and the result, in order:

| Guard | Catches |
| --- | --- |
| `accepted-mismatch` | A challenge captured against a different job, replayed against this one |
| facilitator `verify` | A signed transfer that disagrees with the stated requirements |
| `payer-mismatch` | Someone paying for a job they did not commission |
| spend policy | The real price against caps, budget, velocity, abandonment |

An unreachable facilitator **denies**. It never passes through unsettled, and its error class is
distinct from a policy denial so a caller retries rather than changing its budget.

## State

All state is in memory. Nothing is custodial, so a restart costs at most the jobs in flight — and
every settlement is on Hedera regardless. Provider registrations, quotes and jobs are `sync.Mutex`
guarded; expiry and reuse are checked inside the same lock that spends a quote, so two concurrent
dispatches cannot both spend one.

A persistent store is the obvious next step and changes no interface: `internal/store` is the only
package that would move.

## Reading the code

Start at `registry/cmd/registry/quote.go` — it is the newest handler and the shortest path through
the whole design: buyer identity, provider lookup, the hub round trip, a decline, the policy check,
and the audit write, in about a hundred lines. `internal/hub/hub.go` then shows the frame protocol
underneath it, and `internal/store/store.go` shows why a quote can only be spent once.
