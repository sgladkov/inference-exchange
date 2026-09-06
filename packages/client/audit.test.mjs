import { test } from 'node:test';
import assert from 'node:assert/strict';
import {
  readDecisions,
  decodeRecord,
  filterDecisions,
  describeDecision,
  discoverTopic,
} from './audit.mjs';

const b64 = (o) => Buffer.from(JSON.stringify(o)).toString('base64');

const mirrorWith = (messages) => async (url) => ({
  ok: true,
  status: 200,
  json: async () => ({ messages }),
  _url: String(url),
});

const RECORDS = [
  {
    v: 1,
    decision: 'DENY',
    phase: 'dispatch',
    rule: 'per-call-cap',
    reason: 'FIREWALL_DENIED {"rule":"per-call-cap","limit":10000}',
    buyer: '0.0.1001',
    amount_tinybar: 23001,
    settled: { tx: '' },
    declared: { provider_id: 'prov-1' },
  },
  {
    v: 1,
    decision: 'SETTLED',
    phase: 'collect',
    job_id: 'job-1',
    buyer: '0.0.1001',
    amount_tinybar: 24,
    settled: { tx: '0.0.7162784@1.2', payer: '0.0.1001', network: 'hedera:testnet' },
    declared: { provider_id: 'prov-1', reported_units: 5000 },
  },
  { v: 1, decision: 'ALLOW', phase: 'dispatch', buyer: '0.0.2002', settled: { tx: '' }, declared: {} },
];

const asMessages = (records) =>
  records.map((r, i) => ({
    message: b64(r),
    consensus_timestamp: `${1788622868 + i}.000000000`,
    sequence_number: i + 1,
  }));

// --- decoding --------------------------------------------------------------

test('decodeRecord reads a base64 record', () => {
  const got = decodeRecord(b64({ decision: 'DENY', rule: 'velocity' }));
  assert.equal(got.decision, 'DENY');
  assert.equal(got.rule, 'velocity');
});

// A topic without a submit key accepts anything. Foreign messages must be skipped rather than
// crash the reader — and, importantly, they are not evidence of anything.
test('decodeRecord ignores messages that are not decision records', () => {
  assert.equal(decodeRecord(Buffer.from('hello there').toString('base64')), null);
  assert.equal(decodeRecord(b64({ something: 'else' })), null, 'no decision field');
  assert.equal(decodeRecord(b64(['an array'])), null);
  assert.equal(decodeRecord('!!!not base64!!!'), null);
});

test('readDecisions decodes and stamps consensus order', async () => {
  const got = await readDecisions({
    topicId: '0.0.999',
    fetch: mirrorWith(asMessages(RECORDS)),
  });
  assert.equal(got.length, 3);
  assert.equal(got[0].decision, 'DENY');
  assert.ok(got[0].consensus_timestamp, 'no consensus timestamp attached');
  assert.equal(got[0].sequence, 1);
});

test('readDecisions skips foreign messages without failing', async () => {
  const messages = [
    ...asMessages([RECORDS[0]]),
    { message: Buffer.from('somebody else wrote this').toString('base64'), consensus_timestamp: '1.0' },
    ...asMessages([RECORDS[1]]),
  ];
  const got = await readDecisions({ topicId: '0.0.999', fetch: mirrorWith(messages) });
  assert.equal(got.length, 2, 'a foreign message should be skipped, not counted or thrown on');
});

test('readDecisions asks the mirror node for newest first', async () => {
  let seen;
  await readDecisions({
    topicId: '0.0.999',
    limit: 25,
    fetch: async (url) => {
      seen = String(url);
      return { ok: true, status: 200, json: async () => ({ messages: [] }) };
    },
  });
  assert.match(seen, /topics\/0\.0\.999\/messages/);
  assert.match(seen, /limit=25/);
  assert.match(seen, /order=desc/);
});

test('readDecisions requires a topic and surfaces mirror errors', async () => {
  await assert.rejects(() => readDecisions({}), /topicId is required/);
  await assert.rejects(
    () => readDecisions({ topicId: '0.0.999', fetch: async () => ({ ok: false, status: 404 }) }),
    /404/,
  );
});

// --- filtering -------------------------------------------------------------

// The log is public and holds every buyer's decisions, so a buyer's own view must be filtered
// rather than assumed.
test('filterDecisions narrows to one buyer', () => {
  const mine = filterDecisions(RECORDS, { buyer: '0.0.1001' });
  assert.equal(mine.length, 2);
  assert.ok(mine.every((r) => r.buyer === '0.0.1001'));
});

test('filterDecisions narrows by job and by decision kind', () => {
  assert.equal(filterDecisions(RECORDS, { jobId: 'job-1' }).length, 1);
  assert.equal(filterDecisions(RECORDS, { decision: 'DENY' }).length, 1);
  assert.equal(filterDecisions(RECORDS, { buyer: '0.0.1001', decision: 'ALLOW' }).length, 0);
  assert.equal(filterDecisions(RECORDS, {}).length, 3, 'no filter means everything');
});

// --- rendering -------------------------------------------------------------

test('describeDecision names the rule that caused a refusal', () => {
  const text = describeDecision(RECORDS[0]);
  assert.match(text, /DENY/);
  assert.match(text, /per-call-cap/);
  assert.match(text, /23001 tinybar/);
});

// The rendering must preserve the split the record structure enforces: a transaction id is a
// settled fact, a unit count is a provider's claim, and a reader must not have to guess which.
test('describeDecision keeps settled facts apart from provider claims', () => {
  const text = describeDecision(RECORDS[1]);
  assert.match(text, /settled: 0\.0\.7162784@1\.2/);
  assert.match(text, /claimed 5000 units/);
  assert.match(text, /self-reported, unverified/);
});

test('describeDecision renders a consensus timestamp as a date', () => {
  const text = describeDecision({ ...RECORDS[1], consensus_timestamp: '1788622868.909822196' });
  assert.match(text, /2026-/, `got: ${text}`);
});

// --- discovery -------------------------------------------------------------

test('discoverTopic reads the topic off the registry health endpoint', async () => {
  const topic = await discoverTopic('http://registry.test', async () => ({
    ok: true,
    json: async () => ({ status: 'ok', hcs_topic: '0.0.10380084' }),
  }));
  assert.equal(topic, '0.0.10380084');
});

test('discoverTopic returns empty when the registry publishes no log', async () => {
  const topic = await discoverTopic('http://registry.test', async () => ({
    ok: true,
    json: async () => ({ status: 'ok', hcs_topic: '' }),
  }));
  assert.equal(topic, '');
});
