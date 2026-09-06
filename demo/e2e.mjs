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
import { spawn } from 'node:child_process';
import { fileURLToPath } from 'node:url';
import {
  createClient,
  PolicyError,
  DeclinedError,
  QuoteError,
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

/** Quote a prompt and accept the price. Two calls, because that is now the shape of buying. */
async function buy(prompt) {
  const q = await c.quote(p.provider_id, prompt);
  return await c.dispatch(p.provider_id, q.quote_id);
}

// Two real quotes, kept because the cap test below solves for a prompt size using them. Measuring
// the provider's pricing rather than assuming it keeps this working across backends and rates.
const SHORT = 'Say hello.';
const LONG = 'x'.repeat(4000);
let shortQuote;
let longQuote;

await check('a quote prices this prompt, and a longer one costs more', async () => {
  shortQuote = await c.quote(p.provider_id, SHORT);
  longQuote = await c.quote(p.provider_id, LONG);

  assert(shortQuote.quote_id, 'no quote id');
  assert(shortQuote.price_tinybar === shortQuote.estimate_units * p.rate_per_unit,
    'price does not match the advertised rate');
  // The point of the round trip: the provider read this prompt and priced it, so a longer task is
  // a dearer one. A per-unit rate card alone could never have said this.
  assert(longQuote.estimate_units > shortQuote.estimate_units,
    `4000 chars quoted ${longQuote.estimate_units}, 10 chars quoted ${shortQuote.estimate_units}`);
  return `${shortQuote.price_tinybar} vs ${longQuote.price_tinybar} tinybar`;
});

await check('quoting costs nothing and commissions nothing', async () => {
  // Ten quotes, no jobs. If quoting were doing the work, this is where it would show.
  const before = await c.spend();
  await Promise.all(Array.from({ length: 10 }, () => c.quote(p.provider_id, 'A task I will not buy.')));
  const after = await c.spend();
  assert(after.lifetime.settled_tinybar === before.lifetime.settled_tinybar,
    'quoting moved money');
  return `10 quotes, ${after.lifetime.settled_tinybar - before.lifetime.settled_tinybar} tinybar spent`;
});

await check('several providers price the same prompt independently', async () => {
  const ids = providers.map((x) => x.provider_id);
  if (ids.length < 2) return `skipped (only ${ids.length} provider online)`;
  const all = await c.quoteAll(ids, 'Summarise the trade-offs in deferred settlement.');
  const priced = all.filter((q) => !q.declined);
  assert(priced.length >= 1, 'nobody quoted');
  const line = all
    .map((q) => (q.declined ? `${q.provider_id}=declined` : `${q.provider_id}=${q.price_tinybar}`))
    .join('  ');
  // Not asserting the prices differ — they legitimately may not. What matters is that each
  // provider answered for itself, which is the comparison that checks a padded estimate.
  return line;
});

// --- 1. falsifying branches, before anything succeeds ----------------------

console.log('\ndenials (each must fire before its matching success)');

await check('an unpaid collect is refused with a payable challenge', async () => {
  const job = await buy('A short warm-up task.');
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

await check('a per-call cap refuses at quote time, before any work is asked for', async () => {
  // Derived from the registry's own reported cap and from this provider's measured pricing, rather
  // than hard-coded: limits are configurable and backends price differently, so a demo that assumes
  // either number quietly stops testing anything.
  const { per_call_cap_tinybar: cap } = await c.spend();
  if (!cap) return 'skipped (no per-call cap configured)';

  const perChar =
    (longQuote.estimate_units - shortQuote.estimate_units) / (LONG.length - SHORT.length);
  assert(perChar > 0, 'this provider does not price by prompt size; cannot size the test prompt');
  const needUnits = Math.ceil(cap / p.rate_per_unit) + 1000;
  const chars = Math.ceil((needUnits - shortQuote.estimate_units) / perChar) + SHORT.length;
  if (chars > 4_000_000) return `skipped (would need a ${chars}-char prompt)`;

  try {
    await c.quote(p.provider_id, 'y'.repeat(chars));
    throw new Error('the quote was allowed');
  } catch (e) {
    assert(e instanceof PolicyError, `got ${e.name}: ${e.message}`);
    assert(e.rule === 'per-call-cap', `rule was ${e.rule}`);
    return `${e.rule} on a ${chars}-char prompt`;
  }
});

await check('a quote is single-use', async () => {
  // Otherwise one accepted price buys unlimited work: the quote is the agreement, and an agreement
  // that can be replayed is not one.
  const q = await c.quote(p.provider_id, 'Spend me once.');
  await c.dispatch(p.provider_id, q.quote_id);
  try {
    await c.dispatch(p.provider_id, q.quote_id);
    throw new Error('the quote was spent twice');
  } catch (e) {
    assert(e instanceof QuoteError, `got ${e.name}: ${e.message}`);
    return 'second dispatch refused';
  }
});

await check('another buyer cannot spend this buyer\'s quote', async () => {
  const q = await c.quote(p.provider_id, 'Not yours to accept.');
  const res = await fetch(`${opt.registry}/p/${p.provider_id}/job`, {
    method: 'POST',
    headers: { 'content-type': 'application/json', 'X-Buyer-Account': '0.0.999999' },
    body: JSON.stringify({ quote_id: q.quote_id }),
  });
  assert(res.status === 409 || res.status === 404, `status ${res.status}`);
  return String(res.status);
});

await check('a prompt cannot be swapped in after it was priced', async () => {
  // The cheap-quote-expensive-job swap. The prompt lives with the quote and dispatch has nowhere to
  // put another one, so this asserts the registry ignores a smuggled field rather than honouring it.
  const q = await c.quote(p.provider_id, SHORT);
  const res = await fetch(`${opt.registry}/p/${p.provider_id}/job`, {
    method: 'POST',
    headers: { 'content-type': 'application/json', 'X-Buyer-Account': buyer },
    body: JSON.stringify({ quote_id: q.quote_id, prompt: LONG, max_units: 1_000_000 }),
  });
  assert(res.status === 202, `status ${res.status}`);
  const job = await res.json();
  assert(job.agreed_units === q.estimate_units,
    `job ran at ${job.agreed_units} units against a quote of ${q.estimate_units}`);
  const done = await c.waitFor(job.job_id, { pollMs: 200 });
  assert(done.price_tinybar <= q.price_tinybar,
    `paid ${done.price_tinybar} against a quote of ${q.price_tinybar}`);
  await c.collect(job.job_id);
  return `ran the quoted prompt at ${job.agreed_price_tinybar} tinybar`;
});

await check('a replayed challenge from another job is refused', async () => {
  const a = await buy('Job A.');
  const b = await buy('A rather longer job B, to make its price differ.');
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
    p.provider_id, 'Say hello in exactly five words.',
    (e) => phases.push(e.phase), { pollMs: 200 },
  );
  assert(out.result, 'no result returned');
  assert(out.tx_id, 'no transaction id');
  // `quoting` first, because the price is agreed before anything is commissioned; `running` because
  // dispatch returns before the work does — real progress, not a heartbeat invented by the client.
  assert(phases[0] === 'quoting', `first phase was ${phases[0]}`);
  assert(phases.includes('quoted'), 'the agreed price was never reported');
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

const rigged = [];

/** Start a provider with rigged answers and wait for it to announce its id. */
function startRiggedProvider(account, args) {
  const script = fileURLToPath(new URL('./rigged-provider.mjs', import.meta.url));
  const proc = spawn(process.execPath, [script, '--registry', opt.registry, '--account', account, ...args]);
  rigged.push(proc);
  return new Promise((resolve, reject) => {
    const timer = setTimeout(() => reject(new Error('it never connected')), 15_000);
    proc.stdout.on('data', (b) => {
      const m = /provider_id (\S+)/.exec(String(b));
      if (m) {
        clearTimeout(timer);
        resolve(m[1]);
      }
    });
    proc.stderr.on('data', (b) => {
      clearTimeout(timer);
      reject(new Error(String(b).slice(0, 200)));
    });
  });
}

await check('an inflated unit report is clamped to the quote the provider gave', async () => {
  // A buyer can no longer produce an overrun by naming a low ceiling — the ceiling comes from the
  // seller now. So the only party who can still create one is the seller, and this starts one that
  // does: it quotes 10 units and then claims 5000. It must be paid for 10.
  const seller = process.env.SELLER_ID;
  if (!seller) return 'skipped (SELLER_ID not set)';

  const liar = await startRiggedProvider(seller, ['--quote', '10', '--report', '5000']);
  try {
    const q = await c.quote(liar, 'Work I will underquote.');
    assert(q.estimate_units === 10, `quoted ${q.estimate_units}, expected the rigged 10`);

    const dispatched = await c.dispatch(liar, q.quote_id);
    const job = await c.waitFor(dispatched.job_id, { pollMs: 200 });

    assert(job.reported_units === 5000, `reported ${job.reported_units}`);
    assert(job.priced_units === 10, `priced ${job.priced_units}, want the quoted 10`);
    assert(job.price_tinybar === q.price_tinybar,
      `price ${job.price_tinybar} does not match the quoted ${q.price_tinybar}`);
    return `claimed ${job.reported_units}, paid for ${job.priced_units} — the overrun is the provider's loss`;
  } finally {
    rigged.pop()?.kill();
  }
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
  const out = await c.delegate(p.provider_id, 'sleep:200', () => {}, { pollMs: 2000 });
  const seconds = Math.round((Date.now() - started) / 1000);
  assert(out.tx_id, 'no settlement');
  assert(seconds > 180, `job took ${seconds}s, which does not exercise the window`);
  return `${seconds}s job settled — ${out.tx_id}`;
});

await check('a job survives the caller walking away, and can be reclaimed by id', async () => {
  // Dispatch, drop the client, then come back with only the job id. Under the synchronous shape
  // this was impossible: the id never reached the caller until the work was already done.
  const job = await buy('Work that outlives its requester.');
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
  // Refused at the quote, which is where a buyer now first touches a provider: there is nobody to
  // ask for a price, so there is nothing to accept.
  const res = await fetch(`${opt.registry}/p/${offline.provider_id}/quote`, {
    method: 'POST',
    headers: { 'content-type': 'application/json', 'X-Buyer-Account': buyer },
    body: JSON.stringify({ prompt: 'x' }),
  });
  assert(res.status === 503, `status ${res.status}, want 503`);
  return '503';
});

await check('a provider may refuse work, and refusing is not failing', async () => {
  // The echo backend takes everything, so a refusing provider is started for this. A decline has to
  // be visibly distinct from a failure: same transport, different meaning, and only one of them is
  // the provider's fault.
  const seller = process.env.SELLER_ID;
  if (!seller) return 'skipped (SELLER_ID not set)';

  const reason = 'I do not do image work';
  const picky = await startRiggedProvider(seller, ['--decline', reason]);
  try {
    await c.quote(picky, 'Draw me a cat.');
    throw new Error('the quote was accepted');
  } catch (e) {
    assert(e instanceof DeclinedError, `got ${e.name}: ${e.message}`);
    assert(!(e instanceof ProviderError), 'a refusal was classed as a breakage');
    assert(e.reason.includes(reason), `reason lost in transit: ${e.reason}`);
    assert(e.providerId === picky, 'the decline did not name its provider');
    return `${e.providerId} declined, reason intact`;
  } finally {
    rigged.pop()?.kill();
  }
});

// --- summary ---------------------------------------------------------------

console.log(`\n${passed} passed, ${failed} failed`);
if (settled) {
  console.log(`\nsettlement: ${settled.tx_id}`);
  console.log(`hashscan:   https://hashscan.io/testnet/transaction/${settled.tx_id}`);
}
process.exit(failed === 0 ? 0 : 1);
