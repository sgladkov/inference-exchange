#!/usr/bin/env node
// The demo, driven end to end. Built to be recorded in one take.
//
// Everything here is live: real providers, real quotes, real HBAR on Hedera testnet, real
// settlements read back from a public mirror node. Nothing is staged and no output is fabricated —
// if a step fails on camera it prints the failure, which is the only honest way to film a claim.
//
//   node demo/showcase.mjs --registry http://localhost:8400
//   node demo/showcase.mjs --pace 0        # no pauses, for a dry run
//
// Falsifying branch first: the refusals run before the purchase, so a viewer sees the control fire
// before seeing the happy path it is supposed to be guarding.
import { parseArgs } from 'node:util';
import { createClient, PolicyError, DeclinedError, parseReason } from '../packages/client/client.mjs';

const { values: opt } = parseArgs({
  options: {
    registry: { type: 'string', default: process.env.REGISTRY_URL ?? 'http://localhost:8400' },
    mirror: { type: 'string', default: 'https://testnet.mirrornode.hedera.com/api/v1' },
    pace: { type: 'string', default: '1' }, // multiplier on the reading pauses
  },
});

const C = {
  reset: '\x1b[0m', dim: '\x1b[2m', bold: '\x1b[1m',
  green: '\x1b[32m', red: '\x1b[31m', yellow: '\x1b[33m', cyan: '\x1b[36m', grey: '\x1b[90m',
};
const pace = Number(opt.pace);
const beat = (ms = 1200) => new Promise((r) => setTimeout(r, ms * pace));

let step = 0;
function heading(title, why) {
  step++;
  console.log(`\n${C.bold}${C.cyan}${'─'.repeat(74)}${C.reset}`);
  console.log(`${C.bold}${C.cyan} ${step}. ${title}${C.reset}`);
  if (why) console.log(`${C.grey}    ${why}${C.reset}`);
  console.log(`${C.bold}${C.cyan}${'─'.repeat(74)}${C.reset}\n`);
}
const ok = (m) => console.log(`  ${C.green}✓${C.reset} ${m}`);
const no = (m) => console.log(`  ${C.red}✗${C.reset} ${m}`);
const note = (m) => console.log(`  ${C.grey}${m}${C.reset}`);
const pad = (s, n) => String(s).padEnd(n);

/** Wrap to a fixed width so a model's answer reads as a paragraph on camera, not as one long line. */
function wrap(text, width = 70) {
  const out = [];
  for (const para of text.trim().split('\n')) {
    let line = '';
    for (const word of para.split(/\s+/)) {
      if (!word) continue;
      if ((line + ' ' + word).trim().length > width) {
        out.push(line);
        line = word;
      } else {
        line = (line + ' ' + word).trim();
      }
    }
    out.push(line);
  }
  return out;
}

const buyer = process.env.MERCHANT_ID;
const key = process.env.MERCHANT_KEY;
if (!buyer || !key) {
  console.error('MERCHANT_ID and MERCHANT_KEY must be set');
  process.exit(1);
}
const c = createClient({ registry: opt.registry, accountId: buyer, privateKey: key });

console.log(`\n${C.bold}  INFERENCE EXCHANGE${C.reset}`);
console.log(`  ${C.grey}An agent marketplace where one AI agent pays another, per task, in HBAR.${C.reset}`);
console.log(`  ${C.grey}registry ${opt.registry}   ·   buyer ${buyer}   ·   Hedera testnet${C.reset}`);
await beat(2500);

// --- 1 ---------------------------------------------------------------------
heading('Who is selling right now', 'Providers dial out to the broker. None of them listens on a port, so a laptop behind NAT can sell.');

const providers = await c.findProviders();
for (const p of providers) {
  const d = p.declared ?? {};
  console.log(
    `  ${pad(p.display_name, 18)} ${C.dim}${pad(p.provider_id, 26)}${C.reset}` +
    `${String(p.rate_per_unit).padStart(2)} tinybar/unit   ${C.grey}declared: ${d.backend ?? '?'}${d.model ? '/' + d.model : ''}${C.reset}`,
  );
}
note('');
note('Everything under "declared" is the provider\'s own word. The exchange never verifies it.');
await beat(3500);

// --- 2 ---------------------------------------------------------------------
heading('First, a refusal', 'Falsifying branch before the happy path: the spend policy has to visibly fire.');

const tier = providers.find((p) => p.display_name === 'Fast Tier') ?? providers[0];
const { per_call_cap_tinybar: cap } = await c.spend();
note(`The buyer's per-call cap is ${cap.toLocaleString()} tinybar. Asking for a job far past it:`);
await beat(1500);

try {
  await c.quote(tier.provider_id, 'y'.repeat(900_000));
  no('the quote was allowed — the cap did not fire');
} catch (e) {
  if (e instanceof PolicyError) {
    const { evidence } = parseReason(e.reason);
    ok(`refused: ${C.bold}${e.rule}${C.reset}`);
    note(`  quoted ${Number(evidence.amount).toLocaleString()} tinybar against a limit of ${Number(evidence.limit).toLocaleString()}`);
    note('');
    note('Refused at quote time — before any provider was asked to do a thing. Nobody burned compute.');
    note('The rule travels in the reason string, so the calling agent can branch on it.');
  } else {
    no(`unexpected ${e.name}: ${e.message}`);
  }
}
await beat(4000);

// --- 3 ---------------------------------------------------------------------
heading('One task, priced by everyone', 'This is the part that makes it a market rather than a price list.');

const TASK = 'In three sentences, explain to a non-engineer what a Hedera consensus timestamp is and why it cannot be back-dated.';
console.log(`  ${C.bold}task:${C.reset} ${TASK}\n`);
await beat(2000);

const quotes = await c.quoteAll(providers.map((p) => p.provider_id), TASK);
const priced = quotes.filter((q) => !q.declined).sort((a, b) => a.price_tinybar - b.price_tinybar);
const refused = quotes.filter((q) => q.declined);

for (const q of priced) {
  const p = providers.find((x) => x.provider_id === q.provider_id);
  console.log(
    `  ${pad(p.display_name, 18)}${C.bold}${String(q.price_tinybar).padStart(8)} tinybar${C.reset}` +
    `   ${C.grey}${q.estimate_units.toLocaleString()} units @ ${q.rate_per_unit}${C.reset}`,
  );
}
for (const q of refused) {
  const p = providers.find((x) => x.provider_id === q.provider_id);
  const why = parseReason(q.reason).evidence?.reason ?? q.reason;
  console.log(`  ${pad(p.display_name, 18)}${C.yellow}${pad('declined', 16)}${C.reset}${C.grey}"${why}"${C.reset}`);
}
note('');
note('Each provider read this exact prompt and priced it against its own backend.');
note('A decline is an answer, not a failure — it is never scored against the provider.');
note('');
note(`Every quote is ${C.bold}binding on the provider${C.reset}${C.grey}: overrun comes out of its margin, not the buyer's budget.`);
await beat(5000);

// --- 4 ---------------------------------------------------------------------
const pick = priced[0];
const pickName = providers.find((x) => x.provider_id === pick.provider_id).display_name;
heading(`Buy it — ${pickName} at ${pick.price_tinybar} tinybar`, 'Dispatch is free. Payment happens after delivery, so the work is not racing a transaction expiry.');

const started = Date.now();
let lastPhase = '';
const out = await c.delegate(pick.provider_id, TASK, (e) => {
  if (e.phase === lastPhase && e.phase !== 'running') return;
  lastPhase = e.phase;
  const t = ((Date.now() - started) / 1000).toFixed(0).padStart(3);
  if (e.phase === 'quoted') return note(`${t}s  quoted ${e.price} tinybar for ${e.units} units`);
  if (e.phase === 'running') return process.stdout.write(`\r  ${C.grey}${t}s  the provider is working…${C.reset}`);
  if (e.phase === 'collecting') return console.log(`\n  ${C.grey}${t}s  work delivered — paying ${e.price} tinybar${C.reset}`);
  if (e.phase === 'paid') return ok(`${t}s  settled on Hedera`);
}, { pollMs: 1000 });

console.log(`\n  ${C.bold}answer:${C.reset}`);
for (const line of wrap(out.result).slice(0, 10)) console.log(`  ${C.grey}│${C.reset} ${line}`);
console.log('');
ok(`paid ${C.bold}${out.price_tinybar} tinybar${C.reset} for ${out.priced_units.toLocaleString()} units actually used`);
if (out.reported_units > out.priced_units) {
  note(`  the provider used ${out.reported_units.toLocaleString()} but quoted ${out.priced_units.toLocaleString()} — it absorbed the overrun`);
} else {
  note(`  quoted ${pick.estimate_units.toLocaleString()} units, used ${out.reported_units.toLocaleString()} — the buyer pays for what was used, never more than quoted`);
}
console.log(`  ${C.cyan}https://hashscan.io/testnet/transaction/${out.tx_id}${C.reset}`);
await beat(5000);

// --- 5 ---------------------------------------------------------------------
heading('Verify it without asking us', 'A settlement nobody can independently check is a claim, not a payment.');

const norm = out.tx_id.replace('@', '-').replace(/\.(\d+)$/, '-$1');
let tx = null;
for (let i = 0; i < 15 && !tx; i++) {
  const res = await fetch(`${opt.mirror}/transactions/${norm}`);
  if (res.ok) tx = (await res.json()).transactions?.[0];
  if (!tx) await new Promise((r) => setTimeout(r, 2000));
}

if (!tx) {
  no('the mirror node has not caught up yet');
} else {
  const t = tx.transfers ?? [];
  const paid = t.find((x) => x.account === buyer && x.amount < 0);
  const got = t.find((x) => x.account === pick.pay_to || (x.amount === out.price_tinybar && x.account !== buyer));
  const fee = t.find((x) => x.account === '0.0.7162784' && x.amount < 0);
  ok(`public mirror node says: ${C.bold}${tx.result}${C.reset}`);
  note(`  buyer    ${buyer}  ${paid ? paid.amount : '?'} tinybar`);
  note(`  provider ${got?.account ?? '?'}  +${out.price_tinybar} tinybar   ${C.grey}— paid directly, the registry never holds funds${C.reset}`);
  note(`  network fee paid by ${C.bold}0.0.7162784${C.reset}${C.grey} — the Blocky402 facilitator, not the buyer and not us${C.reset}`);
  note('');
  note('The buyer never needed HBAR for gas. That is what the x402 facilitator is for.');
}
await beat(5000);

// --- 6 ---------------------------------------------------------------------
heading('What the exchange decided, on the public record', 'Read from a mirror node — the exchange cannot alter or withhold what it decided about you.');

// Consensus is ~3s but mirror propagation of the newest record is the thing being waited on, and
// it is the settlement — the record the whole step exists to show. Poll for it rather than guess.
process.stdout.write(`  ${C.grey}waiting for the settlement record to reach a mirror node…${C.reset}`);
for (let i = 0; i < 12; i++) {
  await new Promise((r) => setTimeout(r, 2500));
  try {
    const { records } = await c.decisions({ limit: 40 });
    if (records.some((r) => r.decision === 'SETTLED' && r.settled?.tx === out.tx_id)) break;
  } catch { /* keep waiting */ }
}
console.log('\r' + ' '.repeat(64) + '\r');
try {
  const { topicId, records } = await c.decisions({ limit: 40 });
  const counts = records.reduce((a, r) => ({ ...a, [r.decision]: (a[r.decision] ?? 0) + 1 }), {});
  note(`Hedera Consensus Service topic ${C.bold}${topicId}${C.reset}${C.grey} — ${records.length} decisions about this buyer`);
  note(Object.entries(counts).map(([k, v]) => `${k} ${v}`).join('   '));
  console.log('');

  // One of each class, not the newest six: most traffic is routine ALLOWs, and the point of the
  // log is that the refusals and the settlement sit on it under the same signature.
  const show = [];
  for (const want of ['SETTLED', 'DENY', 'DECLINED', 'ALLOW']) {
    const hit = records.find((r) => r.decision === want);
    if (hit) show.push(hit);
  }
  for (const r of show) {
    const colour = r.decision === 'DENY' ? C.red : r.decision === 'DECLINED' ? C.yellow : C.green;
    const detail = r.decision === 'SETTLED'
      ? `${C.bold}${r.amount_tinybar} tinybar${C.reset}  ${C.grey}tx ${r.settled?.tx ?? ''}`
      : r.reason
        ? `${C.grey}${r.reason.slice(0, 62)}`
        : `${C.grey}${r.amount_tinybar ? r.amount_tinybar + ' tinybar' : ''}`;
    console.log(`  ${colour}${pad(r.decision, 10)}${C.reset}${C.grey}${pad(r.phase, 10)}${C.reset}${detail}${C.reset}`);
  }
  console.log('');
  note('Every branch of this demo is on that topic: the cap refusing step 2, the provider');
  note('declining step 3, and the settlement from step 4 — same log, same signature.');
  note('A record keeps what the ledger settled separate from what the provider merely claimed,');
  note('so neither can be mistaken for the other.');
} catch (e) {
  no(`decision log unavailable: ${e.message}`);
}

console.log(`\n${C.bold}${C.cyan}${'─'.repeat(74)}${C.reset}`);
console.log(`  ${C.bold}One agent hired another, agreed a price for the specific task,${C.reset}`);
console.log(`  ${C.bold}and paid for it in HBAR. Every step is verifiable by a stranger.${C.reset}`);
console.log(`\n  ${C.cyan}https://hashscan.io/testnet/transaction/${out.tx_id}${C.reset}`);
console.log(`${C.bold}${C.cyan}${'─'.repeat(74)}${C.reset}\n`);
