import { test } from 'node:test';
import assert from 'node:assert/strict';
import {
  createClient,
  classify,
  parseReason,
  PolicyError,
  ProviderError,
  UpstreamError,
  ExpiredError,
} from './client.mjs';

// A real testnet key: these tests never settle anything, but createClientHederaSigner parses the
// key at construction, so it has to be well formed.
const KEY = '0x1c8364c6c4107f42a371aff4e1b1d93aa6ef8ea6038f4c4fc25e7da918152038';
const ACCOUNT = '0.0.10123224';

const json = (status, body, headers = {}) => ({
  ok: status >= 200 && status < 300,
  status,
  json: async () => body,
  text: async () => JSON.stringify(body),
  headers: { get: (k) => headers[k.toLowerCase()] ?? null },
});

const b64 = (o) => Buffer.from(JSON.stringify(o)).toString('base64');

function clientWith(handler) {
  const calls = [];
  const c = createClient({
    registry: 'http://registry.test',
    accountId: ACCOUNT,
    privateKey: KEY,
    fetch: async (url, init = {}) => {
      calls.push({ url: String(url), init });
      return handler(String(url), init, calls.length);
    },
  });
  return { c, calls };
}

// --- reason parsing and classification -------------------------------------

test('parseReason splits the prefix from its evidence', () => {
  const { prefix, evidence } = parseReason('FIREWALL_DENIED {"rule":"per-call-cap","limit":10000}');
  assert.equal(prefix, 'FIREWALL_DENIED');
  assert.equal(evidence.rule, 'per-call-cap');
  assert.equal(evidence.limit, 10000);
});

test('parseReason tolerates a bare prefix or broken evidence', () => {
  assert.deepEqual(parseReason('FIREWALL_DENIED'), { prefix: 'FIREWALL_DENIED', evidence: {} });
  assert.deepEqual(parseReason('FIREWALL_DENIED {not json'), {
    prefix: 'FIREWALL_DENIED',
    evidence: {},
  });
  assert.deepEqual(parseReason(''), { prefix: '', evidence: {} });
});

// Each class maps to a different correct action, which is the whole reason the prefixes exist.
test('classify maps each prefix to the action it implies', () => {
  assert.ok(classify('FIREWALL_DENIED {"rule":"daily-budget"}') instanceof PolicyError);
  assert.ok(classify('FIREWALL_UPSTREAM_UNAVAILABLE {"phase":"settle"}') instanceof UpstreamError);
  assert.ok(classify('JOB_FAILED {"detail":"boom"}', 'job-1') instanceof ProviderError);
  assert.ok(classify('JOB_EXPIRED {}', 'job-1') instanceof ExpiredError);
});

test('an unrecognised reason stops rather than retries', () => {
  // The facilitator's own strings land here. Retrying an unknown refusal is the more expensive
  // mistake, so anything unmapped is a stop.
  const err = classify('invalid_exact_hedera_payload_amount_mismatch');
  assert.ok(err instanceof PolicyError);
  assert.ok(!(err instanceof UpstreamError));
});

test('a policy error exposes the rule for branching', () => {
  const err = classify('FIREWALL_DENIED {"rule":"unproven-provider-cap","limit":5000}');
  assert.equal(err.rule, 'unproven-provider-cap');
  assert.equal(err.reason, 'FIREWALL_DENIED {"rule":"unproven-provider-cap","limit":5000}');
});

test('errors carry the job id where one exists', () => {
  assert.equal(classify('JOB_FAILED {}', 'job-7').jobId, 'job-7');
  assert.equal(classify('JOB_EXPIRED {}', 'job-7').jobId, 'job-7');
});

// --- construction ----------------------------------------------------------

test('createClient refuses to start without what it needs', () => {
  const base = { registry: 'http://r', accountId: ACCOUNT, privateKey: KEY };
  assert.throws(() => createClient({ ...base, registry: '' }), /registry/);
  assert.throws(() => createClient({ ...base, accountId: '' }), /accountId/);
  assert.throws(() => createClient({ ...base, privateKey: '' }), /privateKey/);
  assert.doesNotThrow(() => createClient(base));
});

// --- discovery -------------------------------------------------------------

test('findProviders filters online by default', async () => {
  const { c, calls } = clientWith(() => json(200, { providers: [{ provider_id: 'prov-1' }] }));
  await c.findProviders();
  assert.match(calls[0].url, /online=true/);

  await c.findProviders({ capability: 'text-generation', maxRate: 50, onlineOnly: false });
  assert.match(calls[1].url, /capability=text-generation/);
  assert.match(calls[1].url, /max_rate=50/);
  assert.ok(!calls[1].url.includes('online=true'));
});

test('quote prices the ceiling, not a prediction', async () => {
  const { c } = clientWith(() =>
    json(200, { provider_id: 'prov-1', rate_per_unit: 3, pay_to: '0.0.5005', declared: {} }),
  );
  const q = await c.quote('prov-1', 100);
  assert.equal(q.max_price_tinybar, 300);
  assert.equal(q.rate_per_unit, 3);
  assert.equal(q.pay_to, '0.0.5005');
});

// --- dispatch --------------------------------------------------------------

test('dispatch sends the buyer header and the ceiling', async () => {
  const { c, calls } = clientWith(() => json(200, { job_id: 'job-1', billable: true, price_tinybar: 30 }));
  await c.dispatch('prov-1', 'do the thing', 100);

  const body = JSON.parse(calls[0].init.body);
  assert.equal(body.prompt, 'do the thing');
  assert.equal(body.max_units, 100);
  assert.equal(calls[0].init.headers['X-Buyer-Account'], ACCOUNT);
});

test('a policy denial at dispatch is a PolicyError with its rule', async () => {
  const { c } = clientWith(() =>
    json(403, { error: 'FIREWALL_DENIED {"rule":"per-call-cap","amount":50000,"limit":10000}' }),
  );
  await assert.rejects(() => c.dispatch('prov-1', 'p', 100), (e) => {
    assert.ok(e instanceof PolicyError);
    assert.equal(e.rule, 'per-call-cap');
    return true;
  });
});

// A failed job comes back as a 200 with billable:false, because the exchange worked and the
// provider did not. The client must still surface it as an error, and as the right kind.
test('a failed job at dispatch is a ProviderError, not a success', async () => {
  const { c } = clientWith(() =>
    json(200, { job_id: 'job-1', billable: false, error: 'JOB_FAILED {"detail":"backend exploded"}' }),
  );
  await assert.rejects(() => c.dispatch('prov-1', 'p', 100), (e) => {
    assert.ok(e instanceof ProviderError, `got ${e.name}`);
    assert.equal(e.jobId, 'job-1');
    return true;
  });
});

// --- collect ---------------------------------------------------------------

test('collect returns immediately when the job is already paid', async () => {
  const { c, calls } = clientWith(() => json(200, { job_id: 'job-1', result: 'r', tx_id: 'tx-1' }));
  const out = await c.collect('job-1');
  assert.equal(out.tx_id, 'tx-1');
  assert.equal(calls.length, 1, 'an already-collected job should not be paid for again');
});

test('a challenge with no payable options is a denial, not a payment attempt', async () => {
  const reason = 'FIREWALL_DENIED {"rule":"daily-budget","limit":100000}';
  const { c, calls } = clientWith(() =>
    json(402, {}, { 'payment-required': b64({ x402Version: 2, error: reason, accepts: [] }) }),
  );

  await assert.rejects(() => c.collect('job-1'), (e) => {
    assert.ok(e instanceof PolicyError);
    assert.equal(e.rule, 'daily-budget');
    return true;
  });
  assert.equal(calls.length, 1, 'it must not have tried to sign anything');
});

test('a running job is surfaced as a stop, not an empty result', async () => {
  const { c } = clientWith(() => json(202, { error: 'JOB_RUNNING {"job_id":"job-1"}' }));
  await assert.rejects(() => c.collect('job-1'), (e) => {
    // Unmapped prefix, so it stops rather than silently returning nothing.
    assert.ok(e instanceof PolicyError);
    return true;
  });
});

test('an expired job is its own class', async () => {
  const { c } = clientWith(() => json(410, { error: 'JOB_EXPIRED {"job_id":"job-1"}' }));
  await assert.rejects(() => c.collect('job-1'), (e) => {
    assert.ok(e instanceof ExpiredError, `got ${e.name}`);
    assert.equal(e.jobId, 'job-1');
    return true;
  });
});

test('an unreachable facilitator is retryable, not a budget problem', async () => {
  const reason = 'FIREWALL_UPSTREAM_UNAVAILABLE {"phase":"settle"}';
  const { c } = clientWith(() =>
    json(402, {}, { 'payment-required': b64({ x402Version: 2, error: reason, accepts: [] }) }),
  );
  await assert.rejects(() => c.collect('job-1'), (e) => {
    assert.ok(e instanceof UpstreamError, `got ${e.name}`);
    return true;
  });
});

// --- delegate --------------------------------------------------------------

test('delegate reports each phase in order', async () => {
  const accepts = [
    {
      scheme: 'exact',
      network: 'hedera:testnet',
      amount: '30',
      asset: '0.0.0',
      payTo: '0.0.5005',
      maxTimeoutSeconds: 120,
      extra: { feePayer: '0.0.7162784' },
    },
  ];
  const { c } = clientWith((url, init) => {
    if (url.endsWith('/job')) {
      return json(200, { job_id: 'job-1', billable: true, price_tinybar: 30 });
    }
    if (init.headers?.['PAYMENT-SIGNATURE']) {
      return json(200, { job_id: 'job-1', result: 'the answer', tx_id: 'tx-1', price_tinybar: 30 });
    }
    return json(402, {}, { 'payment-required': b64({ x402Version: 2, error: 'Payment required', accepts }) });
  });

  const phases = [];
  const out = await c.delegate('prov-1', 'p', 100, (e) => phases.push(e.phase));

  assert.deepEqual(phases, ['dispatching', 'collecting', 'paid']);
  assert.equal(out.result, 'the answer');
  assert.equal(out.tx_id, 'tx-1');
});

test('delegate stops at dispatch without attempting payment', async () => {
  const { c, calls } = clientWith(() => json(403, { error: 'FIREWALL_DENIED {"rule":"velocity"}' }));
  const phases = [];
  await assert.rejects(() => c.delegate('prov-1', 'p', 100, (e) => phases.push(e.phase)), PolicyError);
  assert.deepEqual(phases, ['dispatching']);
  assert.equal(calls.length, 1);
});
