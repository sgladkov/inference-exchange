#!/usr/bin/env node
// A provider with its answers rigged, for exercising the two outcomes an honest backend will not
// produce on demand.
//
// **Under-quoting.** With binding quotes a buyer can no longer produce an overrun by naming a low
// ceiling — the ceiling comes from the seller now. So the only party who can still create one is
// the seller, by mispricing its own work. It quotes 10 units, claims 5000, and is paid for 10. The
// difference is its own loss, which is the incentive that makes a quote mean anything.
//
// **Declining.** A refusal is a first-class outcome and must not be scored as a failure, but a
// backend that takes every job never demonstrates it.
//
//   node demo/rigged-provider.mjs --registry http://localhost:8080 --account 0.0.x
//   node demo/rigged-provider.mjs --account 0.0.x --decline "I do not do image work"
//
// Prints `provider_id <id>` on stdout once connected, so a script can wait for it.
import { parseArgs } from 'node:util';

const { values: opt } = parseArgs({
  options: {
    registry: { type: 'string', default: process.env.REGISTRY_URL ?? 'http://localhost:8080' },
    account: { type: 'string', default: process.env.PROVIDER_ACCOUNT_ID ?? '' },
    rate: { type: 'string', default: '1' },
    quote: { type: 'string', default: '10' },   // what it says the work will cost
    report: { type: 'string', default: '5000' }, // what it claims to have used
    decline: { type: 'string' },                 // refuse everything, with this reason
    name: { type: 'string' },
  },
});

if (!opt.account) {
  console.error('--account is required');
  process.exit(1);
}
const displayName = opt.name ?? (opt.decline ? 'Refusing Provider' : 'Underquoting Provider');
// Name the rigged behaviour rather than the rigging. Both are honest — this provider really does
// refuse, or really does under-quote — but "rigged" in a listing reads as a staged demo rather than
// as the behaviour being demonstrated.
const declaredBackend = opt.decline ? 'refusing' : 'underquoting';

// Registration retries, because this is usually launched from the same script as the registry and
// the registry takes several seconds to open its HCS topic. The real daemon grew this ladder after
// losing exactly this race; a prop that dies here costs a take, since the decline it demonstrates
// is the thing being filmed.
async function registerWithRetry(attempts = 12, waitMs = 1000) {
  for (let i = 1; ; i++) {
    try {
      const res = await fetch(`${opt.registry}/providers`, {
        method: 'POST',
        headers: { 'content-type': 'application/json' },
        body: JSON.stringify({
          account_id: opt.account,
          display_name: displayName,
          capability: 'text-generation',
          rate_per_unit: Number(opt.rate),
          declared: { backend: declaredBackend, host: 'self-hosted' },
        }),
      });
      if (res.ok) return (await res.json()).provider_id;
      // A 4xx is our request being wrong, and it will stay wrong however long we wait.
      if (res.status < 500) {
        console.error(`registration refused: ${res.status} ${await res.text()}`);
        process.exit(1);
      }
      if (i >= attempts) {
        console.error(`registration failed after ${i} attempts: ${res.status}`);
        process.exit(1);
      }
    } catch (e) {
      // No answer at all — the registry is very likely still starting up.
      if (i >= attempts) {
        console.error(`${opt.registry} never answered after ${i} attempts: ${e.message}`);
        process.exit(1);
      }
    }
    await new Promise((r) => setTimeout(r, waitMs));
  }
}

const id = await registerWithRetry();

const url = `${opt.registry.replace(/^http/, 'ws')}/connect?provider_id=${id}`;
const ws = new WebSocket(url);
const send = (o) => ws.send(JSON.stringify(o));

ws.addEventListener('open', () => console.log(`provider_id ${id}`));
ws.addEventListener('error', (e) => {
  console.error(`socket error: ${e.message ?? e.type}`);
  process.exit(1);
});
ws.addEventListener('message', (ev) => {
  const f = JSON.parse(ev.data);
  if (f.type === 'quote') {
    if (opt.decline) {
      // A decline, not a failure: nothing broke, and the reason is the useful part.
      send({ type: 'declined', quote_id: f.quote_id, error: opt.decline });
      return;
    }
    send({ type: 'quoted', quote_id: f.quote_id, units: Number(opt.quote) });
    return;
  }
  if (f.type === 'job') {
    send({ type: 'accepted', job_id: f.job_id });
    send({
      type: 'result',
      job_id: f.job_id,
      result: `I quoted ${opt.quote} units and am claiming ${opt.report}.`,
      units: Number(opt.report),
    });
  }
});
