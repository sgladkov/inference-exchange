#!/usr/bin/env node
// End-to-end proof against live Hedera testnet.
//
// Falsifying branch first throughout: every denial runs before its matching success, so a pass
// means the control actually fired rather than that the happy path happened to work.
//
//   node demo/e2e.mjs --registry http://localhost:8123
//
// Requires MERCHANT_ID and MERCHANT_KEY in the environment, a running registry, and at least one
// connected provider. Each paid step settles real testnet HBAR.
import { parseArgs } from 'node:util';
import {
  createClient,
  PolicyError,
  ProviderError,
  UpstreamError,
} from '../packages/client/client.mjs';

const { values: opt } = parseArgs({
  options: {
    registry: { type: 'string', default: process.env.REGISTRY_URL ?? 'http://localhost:8080' },
    mirror: {
      type: 'string',
      default: 'https://testnet.mirrornode.hedera.com/api/v1',
    },
  },
});

const FACILITATOR_FEE_PAYER = '0.0.7162784';

let passed = 0;
let failed = 0;

async function check(name, fn) {
  try {
    const detail = await fn();
    passed++;
    console.log(`  ✅ ${name}${detail ? `  — ${detail}` : ''}`);
  } catch (e) {
    failed++;
    console.log(`  ❌ ${name}\n       ${String(e.message).slice(0, 300)}`);
  }
}

function assert(cond, msg) {
  if (!cond) throw new Error(msg);
}

const buyer = process.env.MERCHANT_ID;
const key = process.env.MERCHANT_KEY;
if (!buyer || !key) {
  console.error('MERCHANT_ID and MERCHANT_KEY must be set');
  process.exit(1);
}

const c = createClient({ registry: opt.registry, accountId: buyer, privateKey: key });

/** Read a settlement back from the public mirror node, allowing for propagation lag. */
async function mirrorTx(txId, tries = 12) {
  const norm = txId.replace('@', '-').replace(/\.(\d+)$/, '-$1');
  for (let i = 0; i < tries; i++) {
    const res = await fetch(`${opt.mirror}/transactions/${norm}`);
    if (res.ok) {
      const t = (await res.json()).transactions?.[0];
      if (t) return t;
    }
    await new Promise((r) => setTimeout(r, 2000));
  }
  return null;
}

console.log(`\nInference Exchange — end-to-end proof\nregistry: ${opt.registry}\nbuyer:    ${buyer}\n`);

// --- 0. discovery ----------------------------------------------------------

console.log('discovery');
const providers = await c.findProviders();
if (!providers.length) {
  console.error('no providers online — start a provider daemon first');
  process.exit(1);
}
const p = providers[0];
console.log(`  provider ${p.provider_id}  ${p.rate_per_unit} tinybar/unit  declared=${JSON.stringify(p.declared)}\n`);

await check('a quote prices the ceiling, not a prediction', async () => {
  const q = await c.quote(p.provider_id, 100);
  assert(q.max_price_tinybar === p.rate_per_unit * 100, 'quote does not match the advertised rate');
  return `${q.max_price_tinybar} tinybar max for 100 units`;
});

// --- 1. falsifying branches, before anything succeeds ----------------------

console.log('\ndenials (each must fire before its matching success)');

await check('an unpaid collect is refused with a payable challenge', async () => {
  const job = await c.dispatch(p.provider_id, 'A short warm-up task.', 100);
  await c.waitFor(job.job_id, { pollMs: 200 });
  const res = await fetch(`${opt.registry}/p/job/${job.job_id}`, {
    headers: { 'X-Buyer-Account': buyer },
  });
  assert(res.status === 402, `status ${res.status}, want 402`);
  const header = res.headers.get('payment-required');
  assert(header, 'no PAYMENT-REQUIRED header');
  const ch = JSON.parse(Buffer.from(header, 'base64').toString());
  assert(ch.accepts?.length === 1, 'challenge offered no payable option');
  assert(ch.accepts[0].extra?.feePayer === FACILITATOR_FEE_PAYER, 'wrong fee payer in the challenge');
  const body = await res.text();
  assert(!body.includes('warm-up'), 'the result leaked on an unpaid collect');
  globalThis.__warmJob = job;
  return `402, ${ch.accepts[0].amount} tinybar to ${ch.accepts[0].payTo}`;
});

await check('another buyer cannot see the job at all', async () => {
  const res = await fetch(`${opt.registry}/p/job/${globalThis.__warmJob.job_id}`, {
    headers: { 'X-Buyer-Account': '0.0.999999' },
  });
  assert(res.status === 404, `status ${res.status}, want 404`);
  return '404';
});

await check('a per-call cap refuses before any work is done', async () => {
  // Derived from the registry's own reported cap rather than hard-coded: the limits are
  // configurable, and a demo that assumes a number stops testing anything when it changes.
  const { per_call_cap_tinybar: cap } = await c.spend();
  const huge = Math.ceil((cap * 2) / p.rate_per_unit) + 1000;
  try {
    await c.dispatch(p.provider_id, 'This must never run.', huge);
    throw new Error('the dispatch was allowed');
  } catch (e) {
    assert(e instanceof PolicyError, `got ${e.name}: ${e.message}`);
    assert(e.rule === 'per-call-cap', `rule was ${e.rule}`);
    return `${e.rule}`;
  }
});

await check('a replayed challenge from another job is refused', async () => {
  const a = await c.dispatch(p.provider_id, 'Job A.', 100);
  const b = await c.dispatch(p.provider_id, 'A rather longer job B, to make its price differ.', 100);
  await c.waitFor(a.job_id, { pollMs: 200 });
  await c.waitFor(b.job_id, { pollMs: 200 });

  const resA = await fetch(`${opt.registry}/p/job/${a.job_id}`, { headers: { 'X-Buyer-Account': buyer } });
  const chA = JSON.parse(Buffer.from(resA.headers.get('payment-required'), 'base64').toString());

  // Present A's accepted terms against B's job.
  const forged = Buffer.from(
    JSON.stringify({
      x402Version: 2,
      payload: { transaction: 'CsYBKsMBnotarealtransaction' },
      accepted: chA.accepts[0],
    }),
  ).toString('base64');

  const res = await fetch(`${opt.registry}/p/job/${b.job_id}`, {
    headers: { 'X-Buyer-Account': buyer, 'PAYMENT-SIGNATURE': forged },
  });
  assert(res.status === 402, `status ${res.status}, want 402`);
  const ch = JSON.parse(Buffer.from(res.headers.get('payment-required'), 'base64').toString());
  const rule = JSON.parse(ch.error.slice(ch.error.indexOf(' ') + 1)).rule;
  assert(rule === 'accepted-mismatch', `rule was ${rule}`);
  assert(ch.accepts.length === 0, 'a denial still offered something to pay');
  return rule;
});

await check('an unknown job is not disclosed', async () => {
  const res = await fetch(`${opt.registry}/p/job/job-does-not-exist`, {
    headers: { 'X-Buyer-Account': buyer },
  });
  assert(res.status === 404, `status ${res.status}`);
  return '404';
});

// --- 2. the paid path ------------------------------------------------------

console.log('\nthe paid path');

let settled = null;
await check('dispatch, execute, pay, receive — one call', async () => {
  const phases = [];
  const out = await c.delegate(
    p.provider_id, 'Say hello in exactly five words.', 100,
    (e) => phases.push(e.phase), { pollMs: 200 },
  );
  assert(out.result, 'no result returned');
  assert(out.tx_id, 'no transaction id');
  // `running` appears because dispatch now returns before the work does — real progress, not a
  // heartbeat invented by the client.
  assert(phases[0] === 'dispatching', `first phase was ${phases[0]}`);
  assert(phases.includes('running'), 'no progress was reported while the job ran');
  assert(phases.at(-1) === 'paid', `last phase was ${phases.at(-1)}`);
  settled = out;
  return `${out.price_tinybar} tinybar for ${out.priced_units} units — ${out.tx_id}`;
});

await check('re-collecting does not charge again', async () => {
  const again = await c.collect(settled.job_id);
  assert(again.tx_id === settled.tx_id, 'a second transaction was created');
  return 'same transaction';
});

await check('the settlement resolves on the public mirror node', async () => {
  const t = await mirrorTx(settled.tx_id);
  assert(t, 'not found within the retry window');
  assert(t.result === 'SUCCESS', `result ${t.result}`);

  const transfers = t.transfers ?? [];
  const feePayer = transfers.find((x) => x.account === FACILITATOR_FEE_PAYER);
  assert(feePayer && feePayer.amount < 0, 'the facilitator did not pay the network fee');
  assert(
    transfers.some((x) => x.account === buyer && x.amount === -settled.price_tinybar),
    'the buyer was not debited the settled price',
  );
  assert(
    transfers.some((x) => x.account === p.pay_to && x.amount === settled.price_tinybar),
    'the provider was not credited directly — the registry must never hold funds',
  );
  return `SUCCESS, fee paid by ${FACILITATOR_FEE_PAYER}, ${settled.price_tinybar} tinybar buyer → provider`;
});

// --- 3. metering and delivery ---------------------------------------------

console.log('\nmetering and delivery');

await check('an inflated unit report is clamped to the buyer ceiling', async () => {
  // The echo backend reports roughly one unit per four characters, so a long prompt with a low
  // ceiling forces the clamp.
  const long = 'x'.repeat(2000);
  const dispatched = await c.dispatch(p.provider_id, long, 10);
  const job = await c.waitFor(dispatched.job_id, { pollMs: 200 });
  assert(job.reported_units > job.priced_units, 'the provider did not exceed the ceiling');
  assert(job.priced_units === 10, `priced ${job.priced_units}, want the ceiling of 10`);
  assert(
    job.price_tinybar === 10 * p.rate_per_unit,
    `price ${job.price_tinybar} does not match the clamped units`,
  );
  return `reported ${job.reported_units}, charged for ${job.priced_units}`;
});

await check('the reported count is retained, not discarded', async () => {
  const res = await fetch(`${opt.registry}/p/job/${settled.job_id}/status`, {
    headers: { 'X-Buyer-Account': buyer },
  });
  const j = await res.json();
  assert(typeof j.reported_units === 'number', 'reported units not kept');
  assert(typeof j.priced_units === 'number', 'priced units not kept');
  return 'both figures kept for cross-call comparison';
});

await check('a free status check never leaks the result', async () => {
  const res = await fetch(`${opt.registry}/p/job/${settled.job_id}/status`, {
    headers: { 'X-Buyer-Account': buyer },
  });
  const body = await res.text();
  assert(!body.includes(settled.result), 'status disclosed the paid-for result');
  assert(body.includes('price_tinybar'), 'status should still say what it costs');
  return 'price yes, content no';
});

// --- 4. degraded modes -----------------------------------------------------

console.log('\ndegraded modes');

await check('a job longer than the 180s validity window still settles', async () => {
  // The whole reason payment is deferred: no transaction is in flight while the work runs. Skipped
  // unless explicitly requested, since it takes over three minutes.
  if (!process.env.E2E_LONG_JOB) return 'skipped (set E2E_LONG_JOB=1 to run, ~3.5 min)';
  const started = Date.now();
  const out = await c.delegate(p.provider_id, 'sleep:200', 100, () => {}, { pollMs: 2000 });
  const seconds = Math.round((Date.now() - started) / 1000);
  assert(out.tx_id, 'no settlement');
  assert(seconds > 180, `job took ${seconds}s, which does not exercise the window`);
  return `${seconds}s job settled — ${out.tx_id}`;
});

await check('a job survives the caller walking away, and can be reclaimed by id', async () => {
  // Dispatch, drop the client, then come back with only the job id. Under the synchronous shape
  // this was impossible: the id never reached the caller until the work was already done.
  const job = await c.dispatch(p.provider_id, 'Work that outlives its requester.', 100);
  const fresh = createClient({ registry: opt.registry, accountId: buyer, privateKey: key });
  const done = await fresh.waitFor(job.job_id, { pollMs: 200 });
  assert(done.state === 'completed', `state ${done.state}`);
  assert(done.billable === true, 'the reclaimed job is not payable');
  return `reclaimed ${job.job_id}, ${done.price_tinybar} tinybar owing`;
});

await check('an offline provider is refused rather than queued', async () => {
  const all = await c.findProviders({ onlineOnly: false });
  const offline = all.find((x) => !x.online);
  if (!offline) return 'skipped (no offline provider registered)';
  const res = await fetch(`${opt.registry}/p/${offline.provider_id}/job`, {
    method: 'POST',
    headers: { 'content-type': 'application/json', 'X-Buyer-Account': buyer },
    body: JSON.stringify({ prompt: 'x', max_units: 10 }),
  });
  assert(res.status === 503, `status ${res.status}, want 503`);
  return '503';
});

// --- summary ---------------------------------------------------------------

console.log(`\n${passed} passed, ${failed} failed`);
if (settled) {
  console.log(`\nsettlement: ${settled.tx_id}`);
  console.log(`hashscan:   https://hashscan.io/testnet/transaction/${settled.tx_id}`);
}
process.exit(failed === 0 ? 0 : 1);
