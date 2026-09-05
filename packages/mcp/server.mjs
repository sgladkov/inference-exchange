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
  ProviderError,
  UpstreamError,
  ExpiredError,
  StillRunningError,
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
    return `Refused by the exchange's spend policy${rule}. Do not retry this as-is — either lower max_units, pick a cheaper provider, or wait for the budget window to roll over.\n\n${e.reason}`;
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
      title: 'Quote a task before buying it',
      description:
        'Price a task with a named provider. This is the ceiling, not a prediction: you are charged for actual usage, which can be lower.',
      inputSchema: {
        provider_id: z.string().describe('from find_providers'),
        max_units: z.number().int().positive().describe('the most work you will pay for'),
      },
    },
    async ({ provider_id, max_units }) => {
      try {
        const q = await client.quote(provider_id, max_units);
        return reply(
          `Provider ${q.provider_id} charges ${q.rate_per_unit} tinybar/unit, paid to ${q.pay_to}.\n` +
            `At most ${q.max_units} units → at most ${q.max_price_tinybar} tinybar.\n` +
            'Actual usage is priced at collection and is often lower. The ceiling is enforced by the exchange, not by the provider.',
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
        'Send a task to a provider, wait for it, and pay for the result in HBAR on Hedera. Blocks until done. You are only charged if work is delivered — a failed job costs nothing.',
      inputSchema: {
        provider_id: z.string().describe('from find_providers'),
        prompt: z.string().describe('the task to delegate'),
        max_units: z
          .number()
          .int()
          .positive()
          .describe('spending ceiling in units; the exchange enforces it against the provider'),
        wait_seconds: z
          .number()
          .int()
          .positive()
          .optional()
          .describe('how long to wait before handing back a job id (default 600)'),
      },
    },
    async ({ provider_id, prompt, max_units, wait_seconds }, extra) => {
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
        const out = await client.delegate(
          provider_id,
          prompt,
          max_units,
          (e) => {
            if (e.phase === 'dispatching') return void notify('dispatching to the provider', 0);
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

        const clamped =
          out.reported_units > out.priced_units
            ? `\n\nNote: the provider reported ${out.reported_units} units but you were charged for ${out.priced_units} — your ceiling held.`
            : '';
        return reply(
          `${out.result}\n\n---\nPaid ${out.price_tinybar} tinybar for ${out.priced_units} units.\n` +
            `Settled on Hedera testnet: ${out.tx_id}\n` +
            `https://hashscan.io/testnet/transaction/${out.tx_id}${clamped}`,
        );
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
