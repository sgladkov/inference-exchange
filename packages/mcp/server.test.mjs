import { test } from 'node:test';
import assert from 'node:assert/strict';
import { Client } from '@modelcontextprotocol/sdk/client/index.js';
import { InMemoryTransport } from '@modelcontextprotocol/sdk/inMemory.js';
import { buildServer, explain, reply, configFromEnv } from './server.mjs';
import {
  PolicyError,
  DeclinedError,
  QuoteError,
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
  quoteAll: async () => [],
  delegate: async () => ({}),
  decisions: async () => ({ topicId: '0.0.999', records: [] }),
  spend: async () => SPEND,
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

const SPEND = {
  buyer: '0.0.1001',
  day: { spent_tinybar: 120, budget_tinybar: 100000, remaining_tinybar: 99880, jobs: 4 },
  velocity: { calls: 4, limit: 30, window_seconds: 60 },
  per_call_cap_tinybar: 10000,
  lifetime: { settled_tinybar: 120, settled_jobs: 4 },
  abandonment: { abandoned: 0, completed: 4, ratio: 0, limit_ratio: 0.5, judged_after: 4 },
  by_provider: [{ provider_id: 'prov-1', spend_tinybar: 120, calls: 4 }],
  unproven_provider: { cap_tinybar: 5000, proven_at: 10 },
  source: 'registry-side policy state; settlement transaction ids are independently verifiable',
};

// --- the tool surface ------------------------------------------------------

test('exposes exactly the tools that work today', async () => {
  const mcp = await connect(stubClient());
  const names = (await mcp.listTools()).tools.map((t) => t.name).sort();
  assert.deepEqual(names, [
    'compare_quotes',
    'delegate_task',
    'find_providers',
    'get_quote',
    'spend_report',
    'why_blocked',
  ]);
});

test('every tool advertises an introspectable schema', async () => {
  const mcp = await connect(stubClient());
  for (const t of (await mcp.listTools()).tools) {
    assert.ok(t.description, `${t.name} has no description`);
    assert.equal(t.inputSchema?.type, 'object', `${t.name} schema is not introspectable`);
    assert.ok(t.inputSchema.properties, `${t.name} exposes no properties`);
  }
});

test('delegate_task asks for the task, not for a guess at its size', async () => {
  const mcp = await connect(stubClient());
  const tool = (await mcp.listTools()).tools.find((t) => t.name === 'delegate_task');
  assert.deepEqual(tool.inputSchema.required.sort(), ['prompt', 'provider_id']);
  // A caller-supplied ceiling was a guess about someone else's backend, and with no floor under it
  // a low guess bought real work for nothing. The provider's own quote replaced it.
  assert.equal(tool.inputSchema.properties.max_units, undefined);
});

test('get_quote prices a specific prompt rather than a quantity of units', async () => {
  const mcp = await connect(stubClient());
  const tool = (await mcp.listTools()).tools.find((t) => t.name === 'get_quote');
  assert.deepEqual(tool.inputSchema.required.sort(), ['prompt', 'provider_id']);
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

test('get_quote sends the prompt and reports the binding price', async () => {
  let seen;
  const mcp = await connect(
    stubClient({
      quote: async (pid, prompt) => {
        seen = { pid, prompt };
        return {
          quote_id: 'q-1',
          provider_id: 'prov-1',
          rate_per_unit: 3,
          estimate_units: 14200,
          price_tinybar: 42600,
          expires_at: '2026-09-06T12:00:00Z',
        };
      },
    }),
  );
  const text = textOf(
    await mcp.callTool({
      name: 'get_quote',
      arguments: { provider_id: 'prov-1', prompt: 'analyse this design' },
    }),
  );
  assert.equal(seen.prompt, 'analyse this design', 'the provider must price the actual task');
  assert.match(text, /42600 tinybar/);
  assert.match(text, /q-1/);
  // The agent has to know an overrun is not its problem, or it will pad its own request instead.
  assert.match(text, /binding/i);
});

test('get_quote hands a decline back as an answer, naming the provider', async () => {
  const mcp = await connect(
    stubClient({
      quote: async () => {
        throw new DeclinedError('PROVIDER_DECLINED {"reason":"beyond my context window"}', 'prov-1');
      },
    }),
  );
  const res = await mcp.callTool({
    name: 'get_quote',
    arguments: { provider_id: 'prov-1', prompt: 'p' },
  });
  assert.ok(!res.isError);
  const text = textOf(res);
  assert.match(text, /declined/i);
  assert.match(text, /prov-1/);
  assert.match(text, /nothing was charged/i);
  // The next move is another provider, not another attempt at this one.
  assert.match(text, /different provider/i);
});

// --- compare_quotes --------------------------------------------------------

test('compare_quotes ranks real prices and keeps refusals visible', async () => {
  let seen;
  const mcp = await connect(
    stubClient({
      quoteAll: async (ids, prompt) => {
        seen = { ids, prompt };
        return [
          { provider_id: 'prov-1', quote_id: 'q-1', estimate_units: 200, price_tinybar: 600, rate_per_unit: 3 },
          { provider_id: 'prov-2', declined: true, reason: 'PROVIDER_DECLINED {"reason":"too long"}' },
          { provider_id: 'prov-3', quote_id: 'q-3', estimate_units: 80, price_tinybar: 400, rate_per_unit: 5 },
        ];
      },
    }),
  );
  const text = textOf(
    await mcp.callTool({
      name: 'compare_quotes',
      arguments: { provider_ids: ['prov-1', 'prov-2', 'prov-3'], prompt: 'one task' },
    }),
  );

  assert.deepEqual(seen.ids, ['prov-1', 'prov-2', 'prov-3']);
  assert.equal(seen.prompt, 'one task');
  // Cheapest first, and cheapest is not the lowest rate: prov-3 charges more per unit and still
  // wins, which is exactly the comparison a per-unit rate card cannot make.
  assert.ok(text.indexOf('prov-3') < text.indexOf('prov-1'), text);
  assert.match(text, /Cheapest is prov-3 at 400 tinybar/);
  assert.match(text, /prov-2\s+no quote/);
  assert.match(text, /Price is not quality/i);
});

test('compare_quotes says plainly when nobody would take the work', async () => {
  const mcp = await connect(
    stubClient({
      quoteAll: async () => [
        { provider_id: 'prov-1', declined: true, reason: 'too long' },
        { provider_id: 'prov-2', declined: true, reason: 'unproven buyer' },
      ],
    }),
  );
  const text = textOf(
    await mcp.callTool({
      name: 'compare_quotes',
      arguments: { provider_ids: ['prov-1', 'prov-2'], prompt: 'p' },
    }),
  );
  assert.match(text, /No provider quoted/i);
  assert.match(text, /unproven buyer/);
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
      arguments: { provider_id: 'prov-1', prompt: 'do it' },
    }),
  );
  assert.match(text, /the work product/);
  assert.match(text, /24 tinybar for 8 units/);
  assert.match(text, /0\.0\.7162784@1788619787\.308469868/);
  assert.match(text, /hashscan\.io/, 'the agent should be able to hand its user a verifiable link');
});

// An overrun is invisible unless it is said out loud, and it is the one moment the binding quote
// demonstrably did something: the provider ate the difference.
test('delegate_task reports an overrun the provider absorbed', async () => {
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
      arguments: { provider_id: 'prov-1', prompt: 'p' },
    }),
  );
  assert.match(text, /used 5000 units/);
  assert.match(text, /quoted 10/);
  assert.match(text, /absorbed the overrun/);
});

test('delegate_task forwards the prompt and the wait limit, and nothing else', async () => {
  let args;
  const mcp = await connect(
    stubClient({
      delegate: async (pid, prompt, onProgress, opts) => {
        args = { pid, prompt, onProgress, opts };
        return { result: 'r', price_tinybar: 1, priced_units: 1, reported_units: 1, tx_id: 't' };
      },
    }),
  );
  await mcp.callTool({
    name: 'delegate_task',
    arguments: { provider_id: 'prov-1', prompt: 'p', wait_seconds: 30 },
  });
  assert.equal(args.pid, 'prov-1');
  assert.equal(args.prompt, 'p');
  assert.equal(typeof args.onProgress, 'function', 'progress must survive the signature change');
  assert.equal(args.opts.timeoutMs, 30_000);
});

test('delegate_task reports the agreed price alongside what was paid', async () => {
  const mcp = await connect(
    stubClient({
      delegate: async (pid, prompt, onProgress) => {
        onProgress({ phase: 'quoting', provider: pid });
        onProgress({ phase: 'quoted', provider: pid, units: 100, price: 300 });
        onProgress({ phase: 'paid', job: 'job-1' });
        return { result: 'r', price_tinybar: 240, priced_units: 80, reported_units: 80, tx_id: 't' };
      },
    }),
  );
  const text = textOf(
    await mcp.callTool({ name: 'delegate_task', arguments: { provider_id: 'prov-1', prompt: 'p' } }),
  );
  // Under the quote, not at it: the buyer pays for what was used, and can see it did.
  assert.match(text, /Quoted 300 tinybar for 100 units/);
  assert.match(text, /Paid 240 tinybar for 80 units/);
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
    [new DeclinedError('PROVIDER_DECLINED {"reason":"no"}', 'prov-1'), /declined/i, /quote a different provider/i],
    [new QuoteError('QUOTE_INVALID {"detail":"expired"}', 'q-1'), /no longer usable/i, /fresh quote/i],
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
      arguments: { provider_id: 'prov-1', prompt: 'p' },
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

// --- spend_report ----------------------------------------------------------

test('spend_report gives headroom, not just totals', async () => {
  const mcp = await connect(stubClient());
  const text = textOf(await mcp.callTool({ name: 'spend_report', arguments: {} }));
  assert.match(text, /120 tinybar spent/);
  assert.match(text, /remaining 99880/);
  assert.match(text, /prov-1/);
  assert.match(text, /4 of 30 calls/);
});

// An agent that reads a number treats it as a real ceiling, so an unset limit has to read as words.
test('spend_report renders an unset limit as unlimited', async () => {
  const mcp = await connect(
    stubClient({
      spend: async () => ({
        ...SPEND,
        day: { ...SPEND.day, budget_tinybar: 0, remaining_tinybar: -1 },
        per_call_cap_tinybar: 0,
      }),
    }),
  );
  const text = textOf(await mcp.callTool({ name: 'spend_report', arguments: {} }));
  assert.match(text, /budget unlimited/);
  assert.ok(!text.includes('remaining -1'), `a raw -1 leaked: ${text}`);
});

// Below the sample floor the ratio cannot refuse anything, and showing it reads as a warning about
// nothing.
test('spend_report mentions abandonment only once it can bite', async () => {
  const quiet = await connect(
    stubClient({
      spend: async () => ({
        ...SPEND,
        abandonment: { abandoned: 1, completed: 1, ratio: 1, limit_ratio: 0.5, judged_after: 4 },
      }),
    }),
  );
  assert.ok(
    !textOf(await quiet.callTool({ name: 'spend_report', arguments: {} })).includes('Abandonment'),
    'warned about abandonment before the sample floor',
  );

  const loud = await connect(
    stubClient({
      spend: async () => ({
        ...SPEND,
        abandonment: { abandoned: 3, completed: 6, ratio: 0.5, limit_ratio: 0.5, judged_after: 4 },
      }),
    }),
  );
  const text = textOf(await loud.callTool({ name: 'spend_report', arguments: {} }));
  assert.match(text, /Abandonment: 3 of 6/);
  assert.match(text, /wastes a provider/i, 'it should say why this is rate limited');
});

// The registry is reporting on itself here, unlike why_blocked which reads the chain. Saying so is
// the difference between a figure and a claim.
test('spend_report states whose accounting it is', async () => {
  const mcp = await connect(stubClient());
  const text = textOf(await mcp.callTool({ name: 'spend_report', arguments: {} }));
  assert.match(text, /Source: registry-side/);
  assert.match(text, /verifiable/);
});

test('spend_report explains a failure rather than throwing', async () => {
  const mcp = await connect(
    stubClient({
      spend: async () => {
        throw new Error('registry unreachable');
      },
    }),
  );
  const res = await mcp.callTool({ name: 'spend_report', arguments: {} });
  assert.ok(!res.isError);
  assert.match(textOf(res), /registry unreachable/);
});
