#!/usr/bin/env node
// MCP server for the Inference Exchange: lets an agent discover providers and buy work from them.
//
// This is the installable surface. It is advisory by construction — every control that matters
// lives in the registry, which is the only x402 resource server in the system and so the only place
// a policy cannot be routed around. What this server contributes is intent and legibility: which
// provider, for what, and what a refusal means the caller should do next.
//
// Anything written to stdout corrupts the stdio protocol, so all logging goes to stderr.
import { McpServer } from '@modelcontextprotocol/sdk/server/mcp.js';
import { StdioServerTransport } from '@modelcontextprotocol/sdk/server/stdio.js';
import { z } from 'zod';
import {
  createClient,
  PolicyError,
  DeclinedError,
  QuoteError,
  ProviderError,
  UpstreamError,
  ExpiredError,
  StillRunningError,
  describeDecision,
} from '@inference-exchange/client';

export const CONFIG_HELP = `Set these before starting the server:
  REGISTRY_URL   base URL of the exchange registry (default http://localhost:8080)
  MERCHANT_ID    your Hedera account id — the one that pays
  MERCHANT_KEY   its ECDSA private key`;

export function configFromEnv(env = process.env) {
  return {
    registry: env.REGISTRY_URL ?? 'http://localhost:8080',
    accountId: env.MERCHANT_ID ?? '',
    privateKey: env.MERCHANT_KEY ?? '',
  };
}

const log = (...a) => console.error('[inference-exchange]', ...a);

/** A tool result. Errors are returned as values, not thrown: a thrown error reads to the model as
 * a broken tool rather than as an answer it can act on. */
export function reply(text) {
  return { content: [{ type: 'text', text }] };
}

/**
 * Turn a client error into text that says what the agent should do next.
 *
 * This is the payoff for the prefix namespacing that runs through the registry's policy package
 * and the collect handler: a refusal is only useful if the caller can tell "stop" from "try someone
 * else" from "wait and retry".
 */
export function explain(e) {
  if (e instanceof PolicyError) {
    const rule = e.rule ? ` (rule: ${e.rule})` : '';
    return `Refused by the exchange's spend policy${rule}. Do not retry this as-is — either pick a cheaper provider, split the task into smaller ones, or wait for the budget window to roll over.\n\n${e.reason}`;
  }
  if (e instanceof DeclinedError) {
    // Not a failure. The provider read the task and said no, which is an answer — and treating it
    // as an error would push a caller into retrying against a provider that has already refused.
    return `Provider ${e.providerId} declined this task. Nothing broke and nothing was charged; quote a different provider, or reduce what you are asking for.\n\n${e.reason}`;
  }
  if (e instanceof QuoteError) {
    return `That quote is no longer usable — quotes expire, are single-use, and belong to the account that asked for them. Get a fresh quote and dispatch against that.\n\n${e.reason}`;
  }
  if (e instanceof ProviderError) {
    return `The provider failed this job. You were not charged. Try a different provider.\n\n${e.reason}`;
  }
  if (e instanceof UpstreamError) {
    return `The payment facilitator was unreachable, so nothing settled. The work is still held — retry the collection shortly.\n\n${e.reason}`;
  }
  if (e instanceof ExpiredError) {
    return `The result expired before it was collected and is gone. Dispatch the task again.\n\n${e.reason}`;
  }
  if (e instanceof StillRunningError) {
    return `Still running after the wait limit; the job has not failed. Come back to it with job_id ${e.jobId} (state: ${e.state}).`;
  }
  return `Could not complete the request: ${e.message}`;
}

export function buildServer(client, { name = 'inference-exchange', version = '0.1.0' } = {}) {
  const server = new McpServer({ name, version });

  server.registerTool(
    'find_providers',
    {
      title: 'Find inference providers',
      description:
        'List providers currently online in the exchange, with their per-unit price and what they claim to run. Everything under "declared" is provider-asserted and unverified.',
      // inputSchema is a plain object of zod types, not a wrapped z.object({}) — the SDK does the
      // wrapping, and passing z.object produces a schema the client cannot introspect.
      inputSchema: {
        capability: z.string().optional().describe('e.g. text-generation'),
        max_rate: z.number().optional().describe('highest acceptable tinybar per unit'),
      },
    },
    async ({ capability, max_rate }) => {
      try {
        const providers = await client.findProviders({ capability, maxRate: max_rate });
        if (!providers.length) return reply('No providers are online right now.');
        const lines = providers.map(
          (p) =>
            `${p.provider_id}  ${p.rate_per_unit} tinybar/unit  "${p.display_name}"  ` +
            `capability=${p.capability}  declared=${JSON.stringify(p.declared ?? {})}`,
        );
        return reply(
          `${providers.length} provider(s) online:\n\n${lines.join('\n')}\n\n` +
            'The "declared" fields are self-reported by the provider and are not verified by the exchange.',
        );
      } catch (e) {
        return reply(explain(e));
      }
    },
  );

  server.registerTool(
    'get_quote',
    {
      title: 'Price this exact task with one provider',
      description:
        'Ask a provider what this specific prompt would cost it. Nothing is commissioned and nothing is charged. The provider may also decline. The price is binding on the provider: if the work runs over its own estimate, it absorbs the difference.',
      inputSchema: {
        provider_id: z.string().describe('from find_providers'),
        prompt: z.string().describe('the exact task you intend to delegate'),
      },
    },
    async ({ provider_id, prompt }) => {
      try {
        const q = await client.quote(provider_id, prompt);
        return reply(
          `Provider ${q.provider_id} quoted ${q.estimate_units} units → ${q.price_tinybar} tinybar ` +
            `at ${q.rate_per_unit} tinybar/unit.\n` +
            `quote_id ${q.quote_id}, valid until ${q.expires_at}.\n\n` +
            'This price is the provider\'s own and is binding on it: an overrun comes out of its margin, ' +
            'not your budget. Dispatch with delegate_task, which quotes and accepts in one step, or ' +
            'compare_quotes first — a padded estimate is only visible next to someone else\'s.',
        );
      } catch (e) {
        return reply(explain(e));
      }
    },
  );

  server.registerTool(
    'compare_quotes',
    {
      title: 'Price one task with several providers at once',
      description:
        'Ask several providers to price the same prompt and see what each said. Declines come back as answers rather than errors. This is the check on a padded estimate: nobody can see inside a provider\'s pricing, but a buyer can see what its competitors charged for the identical task.',
      inputSchema: {
        provider_ids: z.array(z.string()).describe('from find_providers'),
        prompt: z.string().describe('the exact task, quoted identically by everyone'),
      },
    },
    async ({ provider_ids, prompt }) => {
      try {
        const quotes = await client.quoteAll(provider_ids, prompt);
        const priced = quotes
          .filter((q) => !q.declined)
          .sort((a, b) => a.price_tinybar - b.price_tinybar);
        const refused = quotes.filter((q) => q.declined);

        const lines = priced.map(
          (q) =>
            `${q.provider_id}  ${q.price_tinybar} tinybar  (${q.estimate_units} units @ ${q.rate_per_unit})  quote_id ${q.quote_id}`,
        );
        for (const q of refused) lines.push(`${q.provider_id}  no quote — ${q.reason}`);

        if (!priced.length) {
          return reply(`No provider quoted this task.\n\n${lines.join('\n')}`);
        }
        return reply(
          `${priced.length} quote(s), cheapest first:\n\n${lines.join('\n')}\n\n` +
            `Cheapest is ${priced[0].provider_id} at ${priced[0].price_tinybar} tinybar. ` +
            'Price is not quality: a low quote from a provider with no record is a bet, and a provider ' +
            'that declines is telling you something about the task rather than failing at it.',
        );
      } catch (e) {
        return reply(explain(e));
      }
    },
  );

  server.registerTool(
    'delegate_task',
    {
      title: 'Delegate a task and pay for the result',
      description:
        'Send a task to a provider, wait for it, and pay for the result in HBAR on Hedera. Blocks until done. It quotes first and accepts that quote, so the price is agreed before any work starts; use get_quote or compare_quotes if you want to see the price before committing. You are only charged if work is delivered — a failed or declined job costs nothing.',
      inputSchema: {
        provider_id: z.string().describe('from find_providers'),
        prompt: z.string().describe('the task to delegate'),
        wait_seconds: z
          .number()
          .int()
          .positive()
          .optional()
          .describe('how long to wait before handing back a job id (default 600)'),
      },
    },
    async ({ provider_id, prompt, wait_seconds }, extra) => {
      // Progress notifications need a token from the caller; without one they are simply not sent.
      const token = extra?._meta?.progressToken;
      const notify = async (message, progress) => {
        if (token === undefined) return;
        try {
          await extra.sendNotification({
            method: 'notifications/progress',
            params: { progressToken: token, progress, message },
          });
        } catch {
          // A caller that cannot take progress must not lose the job over it.
        }
      };

      try {
        let lastNotified = 0;
        let quoted = null;
        const out = await client.delegate(
          provider_id,
          prompt,
          (e) => {
            if (e.phase === 'quoting') return void notify('asking the provider for a price', 0);
            if (e.phase === 'quoted') {
              quoted = e;
              return void notify(`quoted ${e.price} tinybar for ${e.units} units`, 0);
            }
            if (e.phase === 'dispatching') return void notify('dispatching at the agreed price', 0);
            if (e.phase === 'running') {
              const secs = Math.round(e.elapsed_ms / 1000);
              // Throttle: polling is per-second but a notification per second is noise.
              if (secs - lastNotified < 5 && secs !== 0) return;
              lastNotified = secs;
              return void notify(`provider working (${secs}s)`, secs);
            }
            if (e.phase === 'collecting') return void notify(`paying ${e.price} tinybar`, undefined);
            if (e.phase === 'paid') return void notify('settled', undefined);
          },
          { timeoutMs: (wait_seconds ?? 600) * 1000 },
        );

        // Worth saying out loud: this is the case the binding quote exists for. The provider needed
        // more work than it priced, and the difference came out of its margin rather than the buyer's.
        const clamped =
          out.reported_units > out.priced_units
            ? `\n\nNote: the provider used ${out.reported_units} units but quoted ${out.priced_units}, ` +
              'so you paid the quoted price and it absorbed the overrun.'
            : '';
        const agreed = quoted ? `Quoted ${quoted.price} tinybar for ${quoted.units} units.\n` : '';
        return reply(
          `${out.result}\n\n---\n${agreed}Paid ${out.price_tinybar} tinybar for ${out.priced_units} units.\n` +
            `Settled on Hedera testnet: ${out.tx_id}\n` +
            `https://hashscan.io/testnet/transaction/${out.tx_id}${clamped}`,
        );
      } catch (e) {
        return reply(explain(e));
      }
    },
  );

  server.registerTool(
    'why_blocked',
    {
      title: 'Explain what the exchange decided, and why',
      description:
        "Read the exchange's decision log for your account: refusals and the rule that caused them, settlements, and provider failures. The log lives on a public Hedera topic and is read from a mirror node, so the exchange cannot alter or withhold what it decided about you.",
      inputSchema: {
        job_id: z.string().optional().describe('narrow to one job'),
        only_refusals: z.boolean().optional().describe('show only denials'),
        limit: z.number().int().positive().optional().describe('how many records to read (default 50)'),
      },
    },
    async ({ job_id, only_refusals, limit }) => {
      try {
        const { topicId, records } = await client.decisions({
          jobId: job_id,
          decision: only_refusals ? 'DENY' : undefined,
          limit: limit ?? 50,
        });

        if (!records.length) {
          return reply(
            `No decisions recorded for your account on topic ${topicId}` +
              (job_id ? ` for job ${job_id}` : '') +
              '.\n\nRecords are published asynchronously and take a few seconds to reach a mirror node, so a very recent decision may not be visible yet.',
          );
        }

        const lines = records.map(describeDecision).join('\n\n');
        return reply(
          `${records.length} decision(s) on topic ${topicId}, newest first:\n\n${lines}\n\n` +
            'These records establish that the exchange wrote each decision at a consensus timestamp. ' +
            'They do not establish that a decision was correct, and because they are written after the ' +
            'fact they do not establish that one preceded the action it describes.',
        );
      } catch (e) {
        return reply(explain(e));
      }
    },
  );

  server.registerTool(
    'spend_report',
    {
      title: 'What you have spent, and what is left',
      description:
        'Your settled spending and the headroom under each of the exchange\'s limits, broken down by provider. Use it before a large task to see whether it will be refused. These are the registry\'s own figures; the settlements behind them are verifiable on a mirror node.',
      inputSchema: {},
    },
    async () => {
      try {
        const r = await client.spend();
        const unlimited = (v) => (v === -1 || v === 0 ? 'unlimited' : v);

        const lines = [
          `Today: ${r.day.spent_tinybar} tinybar spent across ${r.day.jobs} job(s).`,
          `  budget ${unlimited(r.day.budget_tinybar)}, remaining ${unlimited(r.day.remaining_tinybar)}`,
          `Per call: at most ${unlimited(r.per_call_cap_tinybar)} tinybar.`,
          `Rate: ${r.velocity.calls} of ${unlimited(r.velocity.limit)} calls in the last ${r.velocity.window_seconds}s.`,
          `Lifetime: ${r.lifetime.settled_tinybar} tinybar over ${r.lifetime.settled_jobs} settled job(s).`,
        ];

        if (r.by_provider?.length) {
          lines.push('', 'By provider:');
          for (const p of r.by_provider) {
            lines.push(`  ${p.provider_id}  ${p.spend_tinybar} tinybar over ${p.calls} call(s)`);
          }
        }

        // Only worth surfacing once it can actually refuse a dispatch — before the sample floor it
        // is noise that reads like a warning.
        if (r.abandonment.completed >= r.abandonment.judged_after) {
          lines.push(
            '',
            `Abandonment: ${r.abandonment.abandoned} of ${r.abandonment.completed} jobs left uncollected ` +
              `(${(r.abandonment.ratio * 100).toFixed(0)}%, limit ${(r.abandonment.limit_ratio * 100).toFixed(0)}%). ` +
              'Dispatching is free, so leaving work uncollected wastes a provider\'s compute and is rate limited.',
          );
        }

        lines.push('', `Source: ${r.source}.`);
        return reply(lines.join('\n'));
      } catch (e) {
        return reply(explain(e));
      }
    },
  );

  return server;
}

async function main() {
  const cfg = configFromEnv();
  if (!cfg.accountId || !cfg.privateKey) {
    console.error(`inference-exchange MCP server: missing credentials.\n\n${CONFIG_HELP}`);
    process.exit(1);
  }

  const client = createClient(cfg);
  const server = buildServer(client);
  await server.connect(new StdioServerTransport());
  log(`ready — registry ${cfg.registry}, paying from ${cfg.accountId}`);
}

if (import.meta.url === `file://${process.argv[1]}`) {
  main().catch((e) => {
    console.error('[inference-exchange] fatal:', e.message);
    process.exit(1);
  });
}
