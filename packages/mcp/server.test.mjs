import { test } from 'node:test';
import assert from 'node:assert/strict';
import { Client } from '@modelcontextprotocol/sdk/client/index.js';
import { InMemoryTransport } from '@modelcontextprotocol/sdk/inMemory.js';
import { buildServer, explain, reply, configFromEnv } from './server.mjs';
import {
  PolicyError,
  ProviderError,
  UpstreamError,
  ExpiredError,
  StillRunningError,
} from '@inference-exchange/client';

// The exchange client is stubbed: these tests are about the tool surface — what an agent sees and
// what it is told to do next. Payment itself is covered in the client package and the e2e demo.

const stubClient = (overrides = {}) => ({
  findProviders: async () => [],
  quote: async () => ({}),
  delegate: async () => ({}),
  decisions: async () => ({ topicId: '0.0.999', records: [] }),
  ...overrides,
});

async function connect(client) {
  const server = buildServer(client);
  const [clientTransport, serverTransport] = InMemoryTransport.createLinkedPair();
  const mcp = new Client({ name: 'test', version: '0' }, { capabilities: {} });
  await Promise.all([mcp.connect(clientTransport), server.connect(serverTransport)]);
  return mcp;
}

const textOf = (res) => res.content.map((c) => c.text).join('\n');

// --- the tool surface ------------------------------------------------------

test('exposes exactly the tools that work today', async () => {
  const mcp = await connect(stubClient());
  const names = (await mcp.listTools()).tools.map((t) => t.name).sort();
  // spend_report is deliberately still absent: the registry exposes no spend endpoint, and a stub
  // tool that always says "unavailable" costs the agent a turn to discover it is useless.
  assert.deepEqual(names, ['delegate_task', 'find_providers', 'get_quote', 'why_blocked']);
});

test('every tool advertises an introspectable schema', async () => {
  const mcp = await connect(stubClient());
  for (const t of (await mcp.listTools()).tools) {
    assert.ok(t.description, `${t.name} has no description`);
    assert.equal(t.inputSchema?.type, 'object', `${t.name} schema is not introspectable`);
    assert.ok(t.inputSchema.properties, `${t.name} exposes no properties`);
  }
});

test('delegate_task requires the fields that bound spending', async () => {
  const mcp = await connect(stubClient());
  const tool = (await mcp.listTools()).tools.find((t) => t.name === 'delegate_task');
  assert.deepEqual(tool.inputSchema.required.sort(), ['max_units', 'prompt', 'provider_id']);
  // The ceiling must not be optional: it is the buyer's only metering defence.
  assert.ok(tool.inputSchema.properties.max_units);
});

// --- find_providers --------------------------------------------------------

test('find_providers marks declared fields as unverified', async () => {
  const mcp = await connect(
    stubClient({
      findProviders: async () => [
        {
          provider_id: 'prov-1',
          rate_per_unit: 3,
          display_name: 'Echo Tier',
          capability: 'text-generation',
          declared: { backend: 'echo', model: 'claude-opus-5' },
        },
      ],
    }),
  );
  const text = textOf(await mcp.callTool({ name: 'find_providers', arguments: {} }));
  assert.match(text, /prov-1/);
  assert.match(text, /3 tinybar\/unit/);
  // A model name a provider asserts must not read to the agent as a fact the exchange checked.
  assert.match(text, /not verified/i);
});

test('find_providers passes filters through', async () => {
  let seen;
  const mcp = await connect(stubClient({ findProviders: async (o) => ((seen = o), []) }));
  await mcp.callTool({
    name: 'find_providers',
    arguments: { capability: 'text-generation', max_rate: 50 },
  });
  assert.equal(seen.capability, 'text-generation');
  assert.equal(seen.maxRate, 50);
});

test('find_providers says so plainly when nobody is online', async () => {
  const mcp = await connect(stubClient({ findProviders: async () => [] }));
  const text = textOf(await mcp.callTool({ name: 'find_providers', arguments: {} }));
  assert.match(text, /No providers/i);
});

// --- get_quote -------------------------------------------------------------

test('get_quote presents the price as a ceiling, not a prediction', async () => {
  const mcp = await connect(
    stubClient({
      quote: async () => ({
        provider_id: 'prov-1',
        rate_per_unit: 3,
        max_units: 100,
        max_price_tinybar: 300,
        pay_to: '0.0.5005',
      }),
    }),
  );
  const text = textOf(await mcp.callTool({ name: 'get_quote', arguments: { provider_id: 'prov-1', max_units: 100 } }));
  assert.match(text, /at most 300 tinybar/i);
  assert.match(text, /often lower/i);
});

// --- delegate_task ---------------------------------------------------------

test('delegate_task returns the result with its settlement', async () => {
  const mcp = await connect(
    stubClient({
      delegate: async () => ({
        result: 'the work product',
        price_tinybar: 24,
        priced_units: 8,
        reported_units: 8,
        tx_id: '0.0.7162784@1788619787.308469868',
      }),
    }),
  );
  const text = textOf(
    await mcp.callTool({
      name: 'delegate_task',
      arguments: { provider_id: 'prov-1', prompt: 'do it', max_units: 100 },
    }),
  );
  assert.match(text, /the work product/);
  assert.match(text, /24 tinybar for 8 units/);
  assert.match(text, /0\.0\.7162784@1788619787\.308469868/);
  assert.match(text, /hashscan\.io/, 'the agent should be able to hand its user a verifiable link');
});

// The clamp is invisible unless it is said out loud, and it is the one moment the buyer's ceiling
// demonstrably did something.
test('delegate_task reports when the ceiling held', async () => {
  const mcp = await connect(
    stubClient({
      delegate: async () => ({
        result: 'r',
        price_tinybar: 30,
        priced_units: 10,
        reported_units: 5000,
        tx_id: 'tx-1',
      }),
    }),
  );
  const text = textOf(
    await mcp.callTool({
      name: 'delegate_task',
      arguments: { provider_id: 'prov-1', prompt: 'p', max_units: 10 },
    }),
  );
  assert.match(text, /reported 5000 units/);
  assert.match(text, /charged for 10/);
  assert.match(text, /ceiling held/);
});

test('delegate_task forwards the ceiling and the wait limit', async () => {
  let args;
  const mcp = await connect(
    stubClient({
      delegate: async (pid, prompt, maxUnits, _onProgress, opts) => {
        args = { pid, prompt, maxUnits, opts };
        return { result: 'r', price_tinybar: 1, priced_units: 1, reported_units: 1, tx_id: 't' };
      },
    }),
  );
  await mcp.callTool({
    name: 'delegate_task',
    arguments: { provider_id: 'prov-1', prompt: 'p', max_units: 42, wait_seconds: 30 },
  });
  assert.equal(args.maxUnits, 42);
  assert.equal(args.opts.timeoutMs, 30_000);
});

// --- failures are answers, not exceptions ----------------------------------

// A thrown error reads to the model as a broken tool. Every failure must come back as a normal
// result that says what to do instead.
test('every failure class returns advice rather than throwing', async () => {
  const cases = [
    [new PolicyError('FIREWALL_DENIED {"rule":"per-call-cap","limit":10000}'), /spend policy/i, /per-call-cap/],
    [new ProviderError('JOB_FAILED {"detail":"boom"}', 'job-1'), /not charged/i, /different provider/i],
    [new UpstreamError('FIREWALL_UPSTREAM_UNAVAILABLE {"phase":"settle"}'), /nothing settled/i, /retry/i],
    [new ExpiredError('JOB_EXPIRED {}', 'job-1'), /expired/i, /again/i],
    [new StillRunningError('slow', 'job-9', 'running'), /has not failed/i, /job-9/],
  ];

  for (const [err, ...expected] of cases) {
    const mcp = await connect(
      stubClient({
        delegate: async () => {
          throw err;
        },
      }),
    );
    const res = await mcp.callTool({
      name: 'delegate_task',
      arguments: { provider_id: 'prov-1', prompt: 'p', max_units: 10 },
    });
    assert.ok(!res.isError, `${err.name} came back as a protocol error, not an answer`);
    const text = textOf(res);
    for (const re of expected) {
      assert.match(text, re, `${err.name}: ${text}`);
    }
  }
});

test('a policy denial tells the agent not to retry as-is', async () => {
  // Retrying a budget refusal unchanged just burns the caller's turn.
  const text = explain(new PolicyError('FIREWALL_DENIED {"rule":"daily-budget"}'));
  assert.match(text, /Do not retry/i);
});

test('an unexpected error still returns a usable answer', async () => {
  const mcp = await connect(
    stubClient({
      findProviders: async () => {
        throw new Error('the registry is on fire');
      },
    }),
  );
  const res = await mcp.callTool({ name: 'find_providers', arguments: {} });
  assert.ok(!res.isError);
  assert.match(textOf(res), /on fire/);
});

// --- config ----------------------------------------------------------------

test('configFromEnv defaults the registry and requires credentials', () => {
  const empty = configFromEnv({});
  assert.equal(empty.registry, 'http://localhost:8080');
  assert.equal(empty.accountId, '');

  const set = configFromEnv({
    REGISTRY_URL: 'https://exchange.test',
    MERCHANT_ID: '0.0.1',
    MERCHANT_KEY: '0xabc',
  });
  assert.equal(set.registry, 'https://exchange.test');
  assert.equal(set.accountId, '0.0.1');
});

test('reply produces the content shape the protocol expects', () => {
  const r = reply('hello');
  assert.equal(r.content[0].type, 'text');
  assert.equal(r.content[0].text, 'hello');
});

// --- why_blocked -----------------------------------------------------------

const DENIAL = {
  decision: 'DENY',
  phase: 'dispatch',
  rule: 'per-call-cap',
  reason: 'FIREWALL_DENIED {"rule":"per-call-cap","limit":10000}',
  buyer: '0.0.1001',
  amount_tinybar: 23001,
  consensus_timestamp: '1788622868.000000000',
  settled: { tx: '' },
  declared: { provider_id: 'prov-1' },
};

test('why_blocked names the rule and the topic it was read from', async () => {
  const mcp = await connect(
    stubClient({ decisions: async () => ({ topicId: '0.0.10380084', records: [DENIAL] }) }),
  );
  const text = textOf(await mcp.callTool({ name: 'why_blocked', arguments: {} }));
  assert.match(text, /per-call-cap/);
  assert.match(text, /0\.0\.10380084/, 'the agent should be able to check the log itself');
});

test('why_blocked passes its filters through', async () => {
  let seen;
  const mcp = await connect(
    stubClient({
      decisions: async (o) => ((seen = o), { topicId: '0.0.999', records: [] }),
    }),
  );
  await mcp.callTool({
    name: 'why_blocked',
    arguments: { job_id: 'job-7', only_refusals: true, limit: 10 },
  });
  assert.equal(seen.jobId, 'job-7');
  assert.equal(seen.decision, 'DENY');
  assert.equal(seen.limit, 10);
});

// Records are published asynchronously, so "nothing yet" is a real and expected state that must not
// read as "nothing happened".
test('why_blocked explains an empty log rather than implying nothing occurred', async () => {
  const mcp = await connect(stubClient());
  const text = textOf(await mcp.callTool({ name: 'why_blocked', arguments: {} }));
  assert.match(text, /No decisions recorded/);
  assert.match(text, /asynchronously|may not be visible yet/i);
});

// The tool must not overclaim on behalf of the log: consensus ordering is not correctness.
test('why_blocked states what the records do and do not establish', async () => {
  const mcp = await connect(
    stubClient({ decisions: async () => ({ topicId: '0.0.999', records: [DENIAL] }) }),
  );
  const text = textOf(await mcp.callTool({ name: 'why_blocked', arguments: {} }));
  assert.match(text, /consensus timestamp/i);
  assert.match(text, /do not establish that a decision was correct/i);
});

test('why_blocked says so plainly when a registry publishes no log', async () => {
  const mcp = await connect(
    stubClient({
      decisions: async () => {
        throw new Error('this registry publishes no decision log (no hcs_topic at /health)');
      },
    }),
  );
  const res = await mcp.callTool({ name: 'why_blocked', arguments: {} });
  assert.ok(!res.isError);
  assert.match(textOf(res), /no decision log/i);
});
