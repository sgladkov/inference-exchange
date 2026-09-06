# Captured `claude -p --output-format json` payloads

Two real invocations of the same trivial prompt, captured 6 Sept 2026 with Claude Code 2.1.263.
They exist because the `claude-code` backend was originally written against an *assumed* output
shape, which billed 10 units for work that consumed 14,200 tokens — a silent 1400× undercount that
unit tests with a stubbed subprocess could never catch.

| File | State | Total tokens | Cost |
| --- | --- | ---: | ---: |
| `claude-code-cold.json` | first run of a session | 14,200 | $0.065687 |
| `claude-code-warm.json` | cache warm | 14,200 | $0.008250 |

**Total tokens are identical; cost differs 8×.** That is why units are billed on tokens rather than
on `total_cost_usd`: a buyer's ceiling has to mean the same thing regardless of whether the
provider's cache happens to be warm, which is something the buyer cannot see or control.

Both payloads are kept whole rather than trimmed to the fields currently read — guessing which
fields matter is the mistake these fixtures exist to prevent.
