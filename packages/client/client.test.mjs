import { test } from 'node:test';
import assert from 'node:assert/strict';
import {
  createClient,
  classify,
  parseReason,
  PolicyError,
  DeclinedError,
  QuoteError,
  ProviderError,
  UpstreamError,
  ExpiredError,
  StillRunningError,
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

// --- quoting ---------------------------------------------------------------

test('quote sends the prompt and returns the provider\'s own price', async () => {
  const { c, calls } = clientWith(() =>
    json(200, {
      quote_id: 'q-1',
      provider_id: 'prov-1',
      estimate_units: 14200,
      price_tinybar: 42600,
      rate_per_unit: 3,
      expires_at: '2026-09-06T12:00:00Z',
    }),
  );
  const q = await c.quote('prov-1', 'analyse this design');

  assert.match(calls[0].url, /\/p\/prov-1\/quote$/);
  assert.equal(JSON.parse(calls[0].init.body).prompt, 'analyse this design');
  assert.equal(calls[0].init.headers['X-Buyer-Account'], ACCOUNT);
  assert.equal(q.quote_id, 'q-1');
  assert.equal(q.price_tinybar, 42600);
});

test('a decline is a DeclinedError, not a failure', async () => {
  // The distinction the whole quoting round exists to make: this provider read the work and said
  // no. Classifying it as a provider failure would push a caller into retrying against it.
  const { c } = clientWith(() =>
    json(409, { error: 'PROVIDER_DECLINED {"provider":"prov-1","reason":"prompt exceeds my context"}' }),
  );
  await assert.rejects(() => c.quote('prov-1', 'p'), (e) => {
    assert.ok(e instanceof DeclinedError, `got ${e.name}`);
    assert.ok(!(e instanceof ProviderError), 'a refusal must not read as a breakage');
    assert.equal(e.providerId, 'prov-1');
    assert.match(e.reason, /exceeds my context/);
    return true;
  });
});

test('a policy denial at quote time is a PolicyError', async () => {
  // Better here than at dispatch: the buyer learns it cannot afford this before a provider is
  // asked to do anything.
  const { c } = clientWith(() =>
    json(403, { error: 'FIREWALL_DENIED {"rule":"per-call-cap","amount":42600,"limit":30000}' }),
  );
  await assert.rejects(() => c.quote('prov-1', 'p'), (e) => {
    assert.ok(e instanceof PolicyError);
    assert.equal(e.rule, 'per-call-cap');
    return true;
  });
});

test('quoteAll returns one entry per provider, refusals included', async () => {
  // A decline is part of the shopping result. Throwing would lose the two providers that did quote.
  const { c } = clientWith((url) => {
    if (url.includes('prov-1')) return json(200, { quote_id: 'q-1', estimate_units: 100, price_tinybar: 300 });
    if (url.includes('prov-2')) return json(409, { error: 'PROVIDER_DECLINED {"reason":"too long"}' });
    return json(200, { quote_id: 'q-3', estimate_units: 90, price_tinybar: 450 });
  });
  const all = await c.quoteAll(['prov-1', 'prov-2', 'prov-3'], 'the same prompt');

  assert.deepEqual(all.map((q) => q.provider_id), ['prov-1', 'prov-2', 'prov-3']);
  assert.equal(all[0].price_tinybar, 300);
  assert.equal(all[1].declined, true);
  assert.match(all[1].reason, /too long/);
  assert.equal(all[2].price_tinybar, 450);
});

// Competing quotes for one prompt is the only check on a padded estimate, so it must be a single
// round trip rather than a sequence a caller could get bored of.
test('quoteAll asks every provider the identical prompt', async () => {
  const { c, calls } = clientWith(() => json(200, { quote_id: 'q', estimate_units: 1, price_tinybar: 1 }));
  await c.quoteAll(['prov-1', 'prov-2', 'prov-3'], 'one prompt');
  assert.equal(calls.length, 3);
  for (const call of calls) assert.equal(JSON.parse(call.init.body).prompt, 'one prompt');
});

// --- dispatch --------------------------------------------------------------

test('dispatch accepts a quote and never resends the prompt', async () => {
  const { c, calls } = clientWith(() => json(202, { job_id: 'job-1', state: 'running' }));
  await c.dispatch('prov-1', 'q-1');

  const body = JSON.parse(calls[0].init.body);
  assert.equal(body.quote_id, 'q-1');
  // The prompt lives with the quote. Resending it here would let a buyer quote a cheap prompt and
  // dispatch an expensive one against the cheap price.
  assert.equal(body.prompt, undefined);
  assert.equal(body.max_units, undefined, 'the accepted quote is the ceiling; there is no second one');
  assert.equal(calls[0].init.headers['X-Buyer-Account'], ACCOUNT);
});

test('an unusable quote is a QuoteError naming the quote', async () => {
  const { c } = clientWith(() =>
    json(409, { error: 'QUOTE_INVALID {"detail":"quote has expired"}' }),
  );
  await assert.rejects(() => c.dispatch('prov-1', 'q-old'), (e) => {
    assert.ok(e instanceof QuoteError, `got ${e.name}`);
    assert.equal(e.quoteId, 'q-old');
    return true;
  });
});

test('a policy denial at dispatch is a PolicyError with its rule', async () => {
  const { c } = clientWith(() =>
    json(403, { error: 'FIREWALL_DENIED {"rule":"per-call-cap","amount":50000,"limit":10000}' }),
  );
  await assert.rejects(() => c.dispatch('prov-1', 'q-1'), (e) => {
    assert.ok(e instanceof PolicyError);
    assert.equal(e.rule, 'per-call-cap');
    return true;
  });
});

test('dispatch returns a job id without a price', async () => {
  // The price depends on units the provider has not reported yet, so quoting one here would be a
  // guess presented as a fact.
  const { c } = clientWith(() => json(202, { job_id: 'job-1', state: 'running' }));
  const job = await c.dispatch('prov-1', 'q-1');
  assert.equal(job.job_id, 'job-1');
  assert.equal(job.price_tinybar, undefined);
});

// --- waitFor ---------------------------------------------------------------

test('waitFor polls until the job completes', async () => {
  const states = ['running', 'running', 'completed'];
  let i = 0;
  const { c, calls } = clientWith(() =>
    json(200, { job_id: 'job-1', state: states[i++] ?? 'completed', price_tinybar: 30, billable: true }),
  );

  const seen = [];
  const st = await c.waitFor('job-1', { pollMs: 1, onProgress: (e) => seen.push(e.state) });
  assert.equal(st.state, 'completed');
  assert.deepEqual(seen, ['running', 'running', 'completed']);
  assert.equal(calls.length, 3);
});

// A failure surfaces here rather than at dispatch: with an async dispatch this is the first moment
// the outcome is known. Returning it as a value would invite a caller to try collecting it.
test('waitFor raises a ProviderError when the job fails', async () => {
  const { c } = clientWith(() =>
    json(200, { job_id: 'job-1', state: 'failed', billable: false, error: 'the backend exploded' }),
  );
  await assert.rejects(() => c.waitFor('job-1', { pollMs: 1 }), (e) => {
    assert.ok(e instanceof ProviderError, `got ${e.name}`);
    assert.equal(e.jobId, 'job-1');
    assert.match(e.message, /backend exploded/);
    return true;
  });
});

test('waitFor raises an ExpiredError when the result is gone', async () => {
  const { c } = clientWith(() => json(200, { job_id: 'job-1', state: 'expired' }));
  await assert.rejects(() => c.waitFor('job-1', { pollMs: 1 }), ExpiredError);
});

test('waitFor returns an already-collected job rather than looping', async () => {
  const { c, calls } = clientWith(() => json(200, { job_id: 'job-1', state: 'collected', tx_id: 'tx-1' }));
  const st = await c.waitFor('job-1', { pollMs: 1 });
  assert.equal(st.tx_id, 'tx-1');
  assert.equal(calls.length, 1);
});

// Giving up waiting is not the same as the job failing, and must not read as one: the work is
// probably still running and the id is enough to come back to it.
test('waitFor timing out is its own class, not a failure', async () => {
  const { c } = clientWith(() => json(200, { job_id: 'job-1', state: 'running' }));
  await assert.rejects(() => c.waitFor('job-1', { pollMs: 1, timeoutMs: 5 }), (e) => {
    assert.ok(e instanceof StillRunningError, `got ${e.name}`);
    assert.ok(!(e instanceof ProviderError), 'a timeout must not look like a provider failure');
    assert.equal(e.jobId, 'job-1');
    assert.equal(e.state, 'running');
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
    if (url.endsWith('/quote')) {
      return json(200, { quote_id: 'q-1', estimate_units: 10, price_tinybar: 30, rate_per_unit: 3 });
    }
    if (url.endsWith('/job')) return json(202, { job_id: 'job-1', state: 'running' });
    if (url.endsWith('/status')) {
      return json(200, { job_id: 'job-1', state: 'completed', price_tinybar: 30, billable: true });
    }
    if (init.headers?.['PAYMENT-SIGNATURE']) {
      return json(200, { job_id: 'job-1', result: 'the answer', tx_id: 'tx-1', price_tinybar: 30 });
    }
    return json(402, {}, { 'payment-required': b64({ x402Version: 2, error: 'Payment required', accepts }) });
  });

  const phases = [];
  const events = [];
  const out = await c.delegate('prov-1', 'p', (e) => (phases.push(e.phase), events.push(e)), {
    pollMs: 1,
  });

  assert.deepEqual(phases, ['quoting', 'quoted', 'dispatching', 'running', 'collecting', 'paid']);
  // The agreed price is reported before anything is committed to, which is the only moment a
  // caller could still change its mind.
  assert.equal(events[1].price, 30);
  assert.equal(events[1].units, 10);
  assert.equal(out.result, 'the answer');
  assert.equal(out.tx_id, 'tx-1');
});

test('delegate stops at the quote without commissioning anything', async () => {
  // A denial now lands one step earlier than it used to. Nothing has been asked of a provider yet,
  // so the refusal costs nobody any work.
  const { c, calls } = clientWith(() => json(403, { error: 'FIREWALL_DENIED {"rule":"velocity"}' }));
  const phases = [];
  await assert.rejects(
    () => c.delegate('prov-1', 'p', (e) => phases.push(e.phase), { pollMs: 1 }),
    PolicyError,
  );
  assert.deepEqual(phases, ['quoting']);
  assert.equal(calls.length, 1);
  assert.match(calls[0].url, /\/quote$/);
});

test('delegate abandons a task the provider declined, without dispatching', async () => {
  const { c, calls } = clientWith(() =>
    json(409, { error: 'PROVIDER_DECLINED {"reason":"I do not do image work"}' }),
  );
  const phases = [];
  await assert.rejects(
    () => c.delegate('prov-1', 'draw me a cat', (e) => phases.push(e.phase), { pollMs: 1 }),
    DeclinedError,
  );
  assert.deepEqual(phases, ['quoting']);
  assert.equal(calls.length, 1, 'a declined quote must not be dispatched anyway');
});
