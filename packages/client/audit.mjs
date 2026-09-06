// Reading the exchange's decision log back.
//
// The log lives on a Hedera Consensus Service topic, and this reads it from a public mirror node
// rather than from the registry. That is the whole reason for putting it on-chain: a buyer auditing
// the registry should not have to ask the registry what it decided. The registry advertises which
// topic to read at /health, but it cannot alter or withhold what is on it.
//
// What a record proves is narrow: that the exchange wrote this decision at a consensus timestamp.
// Not that the decision was correct, and — because records are submitted after the fact — not that
// it preceded the action it describes.

export const MIRROR_TESTNET = 'https://testnet.mirrornode.hedera.com/api/v1';

/**
 * Fetch and decode decision records from a topic, newest first.
 *
 * @param {object} o
 * @param {string} o.topicId   e.g. "0.0.10380084"
 * @param {string} [o.mirror]  mirror node API base
 * @param {number} [o.limit]   how many messages to read
 */
export async function readDecisions({
  topicId,
  mirror = MIRROR_TESTNET,
  limit = 100,
  fetch: fetchFn = globalThis.fetch,
} = {}) {
  if (!topicId) throw new Error('topicId is required to read the decision log');

  const url = `${mirror}/topics/${topicId}/messages?limit=${limit}&order=desc`;
  const res = await fetchFn(url);
  if (!res.ok) {
    throw new Error(`mirror node ${res.status} reading topic ${topicId}`);
  }
  const body = await res.json();

  const out = [];
  for (const m of body.messages ?? []) {
    const record = decodeRecord(m.message);
    // A topic is public and anyone may submit to it unless it has a submit key. A message that is
    // not one of our records is skipped rather than crashing the reader — and, notably, is not
    // evidence of anything.
    if (record) {
      out.push({ ...record, consensus_timestamp: m.consensus_timestamp, sequence: m.sequence_number });
    }
  }
  return out;
}

/** Decode one base64 mirror-node message, or null if it is not a decision record. */
export function decodeRecord(base64) {
  try {
    const parsed = JSON.parse(Buffer.from(base64, 'base64').toString('utf8'));
    if (!parsed || typeof parsed !== 'object' || !parsed.decision) return null;
    return parsed;
  } catch {
    return null;
  }
}

/** Narrow a record set to one buyer, and optionally one job or one decision kind. */
export function filterDecisions(records, { buyer, jobId, decision } = {}) {
  return records.filter(
    (r) =>
      (!buyer || r.buyer === buyer) &&
      (!jobId || r.job_id === jobId) &&
      (!decision || r.decision === decision),
  );
}

/**
 * Render one record as a line a person or an agent can read.
 *
 * Settled facts and provider claims are kept visibly apart, the same way they are on the wire: a
 * reader must never have to guess which half the ledger stands behind.
 */
export function describeDecision(r) {
  const when = r.consensus_timestamp
    ? new Date(Number(r.consensus_timestamp.split('.')[0]) * 1000).toISOString()
    : (r.at ?? '');
  const bits = [`${when}  ${r.decision}`];
  if (r.phase) bits.push(`at ${r.phase}`);
  if (r.job_id) bits.push(`job ${r.job_id}`);
  if (r.amount_tinybar) bits.push(`${r.amount_tinybar} tinybar`);

  let line = bits.join('  ');
  if (r.decision === 'DENY') {
    line += `\n    refused by rule: ${r.rule}`;
    if (r.reason) line += `\n    ${r.reason}`;
  }
  if (r.decision === 'FAILED' && r.reason) {
    line += `\n    provider failed: ${r.reason}`;
  }
  if (r.settled?.tx) {
    line += `\n    settled: ${r.settled.tx}  (payer ${r.settled.payer})`;
  }
  if (r.declared?.reported_units) {
    line += `\n    provider claimed ${r.declared.reported_units} units — self-reported, unverified`;
  }
  return line;
}

/** Ask a registry which topic carries its decision log. Discovery only. */
export async function discoverTopic(registry, fetchFn = globalThis.fetch) {
  const res = await fetchFn(`${registry}/health`);
  if (!res.ok) throw new Error(`registry health ${res.status}`);
  const h = await res.json();
  return h.hcs_topic || '';
}
