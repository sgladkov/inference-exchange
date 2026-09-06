// The buyer side of the exchange: discover providers, delegate a task, pay for the result.
//
// This is advisory by design. It runs inside a host agent application that must be assumed
// compromised — its task comes from its inputs and its inputs may be attacker-controlled — so it
// holds no policy that matters. Budgets, caps and allowlists live in the registry, which is the
// only x402 resource server in the system and therefore the only place a control cannot be routed
// around. What lives here is intent: which provider, for what, at whose request.
//
// It is also the only component that touches a private key, because it is the only one that signs.
import { x402Client, x402HTTPClient } from '@x402/core/client';
import { ExactHederaScheme } from '@x402/hedera/exact/client';
import { createClientHederaSigner, PrivateKey } from '@x402/hedera';
import { decodePaymentRequiredHeader } from '@x402/core/http';
import { readDecisions, filterDecisions, discoverTopic, MIRROR_TESTNET } from './audit.mjs';

export * from './audit.mjs';

export const NETWORK = 'hedera:testnet';

/** The registry refused. Do not retry: change policy, budget, or provider. */
export class PolicyError extends Error {
  constructor(reason) {
    super(reason);
    this.name = 'PolicyError';
    this.reason = reason;
    this.rule = ruleOf(reason);
  }
}

/** The provider looked at the work and refused it. Nothing broke; quote someone else. */
export class DeclinedError extends Error {
  constructor(reason, providerId) {
    super(reason);
    this.name = 'DeclinedError';
    this.reason = reason;
    this.providerId = providerId;
  }
}

/** The quote is gone, spent, expired, or was never yours. Quote again. */
export class QuoteError extends Error {
  constructor(reason, quoteId) {
    super(reason);
    this.name = 'QuoteError';
    this.reason = reason;
    this.quoteId = quoteId;
  }
}

/** The provider failed. Nothing was charged; try a different provider. */
export class ProviderError extends Error {
  constructor(reason, jobId) {
    super(reason);
    this.name = 'ProviderError';
    this.reason = reason;
    this.jobId = jobId;
  }
}

/** The facilitator or network was unreachable. Retry later; nothing settled. */
export class UpstreamError extends Error {
  constructor(reason) {
    super(reason);
    this.name = 'UpstreamError';
    this.reason = reason;
  }
}

/** The held result expired before it was collected. The work is gone; dispatch again. */
export class ExpiredError extends Error {
  constructor(reason, jobId) {
    super(reason);
    this.name = 'ExpiredError';
    this.reason = reason;
    this.jobId = jobId;
  }
}

/** Waiting gave up before the job did. The job may still be running; come back with its id. */
export class StillRunningError extends Error {
  constructor(reason, jobId, state) {
    super(reason);
    this.name = 'StillRunningError';
    this.reason = reason;
    this.jobId = jobId;
    this.state = state;
  }
}

/**
 * Reasons arrive as `PREFIX {json}` — the reason string is the only field that survives to the
 * caller, so the class token and its evidence are packed into it.
 */
export function parseReason(reason = '') {
  const i = reason.indexOf(' ');
  if (i < 0) return { prefix: reason, evidence: {} };
  try {
    return { prefix: reason.slice(0, i), evidence: JSON.parse(reason.slice(i + 1)) };
  } catch {
    return { prefix: reason.slice(0, i), evidence: {} };
  }
}

function ruleOf(reason) {
  return parseReason(reason).evidence?.rule ?? null;
}

/**
 * Turn a reason string into the error whose type says what to do about it.
 *
 * The whole point of the prefixes: a machine caller must be able to tell "your policy stopped
 * this" from "the provider broke" from "try again later". Anything unrecognised — including the
 * facilitator's own invalid_exact_hedera_* strings — is a policy-class stop rather than a retry,
 * because retrying an unknown refusal is the more expensive mistake.
 */
export function classify(reason, jobId) {
  const { prefix } = parseReason(reason);
  if (prefix === 'FIREWALL_UPSTREAM_UNAVAILABLE') return new UpstreamError(reason);
  if (prefix === 'JOB_FAILED') return new ProviderError(reason, jobId);
  if (prefix === 'JOB_EXPIRED') return new ExpiredError(reason, jobId);
  if (prefix === 'PROVIDER_DECLINED') return new DeclinedError(reason);
  if (prefix === 'QUOTE_FAILED' || prefix === 'QUOTE_INVALID') return new QuoteError(reason, jobId);
  return new PolicyError(reason);
}

/**
 * @param {object} o
 * @param {string} o.registry   base URL of the broker
 * @param {string} o.accountId  buyer's Hedera account — receives nothing, spends everything
 * @param {string} o.privateKey ECDSA key for that account
 */
export function createClient({ registry, accountId, privateKey, fetch: fetchFn = globalThis.fetch }) {
  if (!registry) throw new Error('registry URL is required');
  if (!accountId) throw new Error('accountId is required');
  if (!privateKey) throw new Error('privateKey is required');

  const signer = createClientHederaSigner(accountId, PrivateKey.fromStringECDSA(privateKey));
  const http = new x402HTTPClient(new x402Client().register(NETWORK, new ExactHederaScheme(signer)));
  const headers = { 'content-type': 'application/json', 'X-Buyer-Account': accountId };

  async function findProviders({ capability, maxRate, onlineOnly = true } = {}) {
    const q = new URLSearchParams();
    if (capability) q.set('capability', capability);
    if (maxRate) q.set('max_rate', String(maxRate));
    if (onlineOnly) q.set('online', 'true');
    const res = await fetchFn(`${registry}/providers?${q}`);
    if (!res.ok) throw new Error(`find_providers ${res.status}`);
    return (await res.json()).providers ?? [];
  }

  /**
   * Ask a provider what this specific prompt would cost.
   *
   * Nothing is commissioned. The price comes back from the provider, which is the only party that
   * can judge this prompt against its own backend — and it is **binding**: whatever it estimates
   * becomes the ceiling, and an overrun comes out of the provider's margin rather than the buyer's.
   *
   * Quote several providers for the same prompt to compare real per-task prices. That competition
   * is what checks a provider padding its estimates, since nothing else can see inside one.
   */
  async function quote(providerId, prompt) {
    const res = await fetchFn(`${registry}/p/${providerId}/quote`, {
      method: 'POST',
      headers,
      body: JSON.stringify({ prompt }),
    });
    const body = await res.json().catch(() => ({}));
    if (res.status === 403) throw classify(body.error ?? 'denied at quote');
    if (res.status === 409) throw new DeclinedError(body.error ?? 'the provider declined', providerId);
    if (!res.ok) throw classify(body.error ?? `quote ${res.status}`);
    return body;
  }

  /**
   * Quote one prompt from several providers at once, returning what each said.
   *
   * Declines and denials come back as entries rather than exceptions: a provider refusing is a
   * result the buyer needs in order to choose, not a failure of the shopping trip.
   */
  async function quoteAll(providerIds, prompt) {
    return Promise.all(
      providerIds.map(async (id) => {
        try {
          return { provider_id: id, ...(await quote(id, prompt)) };
        } catch (e) {
          return { provider_id: id, declined: true, reason: e.reason ?? e.message };
        }
      }),
    );
  }

  /**
   * Accept a quote. Returns as soon as the registry accepts it, with a job id — not a result.
   *
   * The prompt is not resent: it came with the quote, and letting a buyer supply it again would
   * allow a swap — quote a cheap prompt, dispatch an expensive one against the cheap price.
   *
   * Free, and asynchronous: no payment is taken here and no Hedera transaction is in flight while
   * the work runs, so the job may take minutes without anything expiring. Holding the id from the
   * outset is what lets a caller report progress and come back after a crash.
   */
  async function dispatch(providerId, quoteId) {
    const res = await fetchFn(`${registry}/p/${providerId}/job`, {
      method: 'POST',
      headers,
      body: JSON.stringify({ quote_id: quoteId }),
    });
    const body = await res.json().catch(() => ({}));
    if (res.status === 403) throw classify(body.error ?? 'denied at dispatch');
    if (res.status === 409) throw new QuoteError(body.error ?? 'quote unusable', quoteId);
    if (res.status !== 202) {
      throw new Error(`dispatch ${res.status}: ${JSON.stringify(body).slice(0, 200)}`);
    }
    return body;
  }

  /** One status read. Free, and it carries neither the prompt nor the result. */
  async function status(jobId) {
    const res = await fetchFn(`${registry}/p/job/${jobId}/status`, { headers });
    if (!res.ok) {
      const body = await res.json().catch(() => ({}));
      throw classify(body.error ?? `status ${res.status}`, jobId);
    }
    return await res.json();
  }

  /**
   * Poll until the job stops moving, then return its final status.
   *
   * A failed or expired job raises here rather than at dispatch, because with an asynchronous
   * dispatch this is where the outcome first becomes known. Returning a failed job as a value would
   * invite a caller to try collecting it.
   */
  async function waitFor(jobId, { onProgress = () => {}, pollMs = 1000, timeoutMs = 30 * 60_000 } = {}) {
    const started = Date.now();
    for (;;) {
      const st = await status(jobId);
      onProgress({ phase: 'running', job: jobId, state: st.state, elapsed_ms: Date.now() - started });

      if (st.state === 'completed' || st.state === 'collected') return st;
      if (st.state === 'failed') {
        throw new ProviderError(st.error ?? 'the provider failed the job', jobId);
      }
      if (st.state === 'expired') {
        throw new ExpiredError(st.error ?? 'the result expired before it was collected', jobId);
      }

      if (Date.now() - started >= timeoutMs) {
        // This bounds our waiting, not the work: the job is very likely still running registry
        // side, and its id is enough to come back to it.
        throw new StillRunningError(
          `gave up waiting on ${jobId} after ${Math.round(timeoutMs / 1000)}s; it is still ${st.state}`,
          jobId,
          st.state,
        );
      }
      await new Promise((r) => setTimeout(r, pollMs));
    }
  }

  /**
   * Collect a completed job, paying for it. One 402, one payment, one result.
   *
   * A failed job never reaches a 402 — the error comes back free — so arriving here with something
   * to pay already means the work was delivered.
   */
  async function collect(jobId) {
    const url = `${registry}/p/job/${jobId}`;
    const first = await fetchFn(url, { headers });

    if (first.status === 200) return await first.json(); // already collected: idempotent
    if (first.status !== 402) {
      const body = await first.json().catch(() => ({}));
      throw classify(body.error ?? `collect ${first.status}`, jobId);
    }

    const challenge = decodePaymentRequiredHeader(first.headers.get('payment-required'));
    // No accepts means "you may not pay for this", as opposed to "here is the price".
    if (!challenge?.accepts?.length) throw classify(challenge?.error ?? 'refused', jobId);

    const payload = await http.createPaymentPayload(challenge);
    const paid = await fetchFn(url, {
      headers: { ...headers, ...http.encodePaymentSignatureHeader(payload) },
    });

    if (paid.status !== 200) {
      // A denial rides in the header's error field: the body is empty and everything else is
      // dropped in transit, so this is the only place a reason can be.
      const h = paid.headers.get('payment-required');
      const reason = h ? decodePaymentRequiredHeader(h)?.error : null;
      throw classify(reason ?? `payment refused (${paid.status})`, jobId);
    }
    return await paid.json();
  }

  /**
   * Read this buyer's decisions out of the exchange's log.
   *
   * Read from a mirror node, not from the registry: the registry says which topic to look at, but
   * cannot alter or withhold what is on it. That independence is the reason the log is on-chain.
   */
  async function decisions({ jobId, decision, topicId, mirror = MIRROR_TESTNET, limit = 100 } = {}) {
    const topic = topicId || (await discoverTopic(registry, fetchFn));
    if (!topic) {
      throw new Error('this registry publishes no decision log (no hcs_topic at /health)');
    }
    const all = await readDecisions({ topicId: topic, mirror, limit, fetch: fetchFn });
    return { topicId: topic, records: filterDecisions(all, { buyer: accountId, jobId, decision }) };
  }

  /**
   * What this buyer has spent, and the headroom under each limit.
   *
   * Unlike `decisions`, this comes from the registry and has no independent source: budgets and
   * velocity are registry-side state by design, which is exactly what makes them enforceable
   * against a buyer who could otherwise edit them. The settlements underneath are checkable on a
   * mirror node; these totals are the registry's own accounting.
   */
  async function spend() {
    const res = await fetchFn(`${registry}/me/spend`, { headers });
    if (!res.ok) {
      const body = await res.json().catch(() => ({}));
      throw classify(body.error ?? `spend ${res.status}`);
    }
    return await res.json();
  }

  /** Quote, accept, wait for the work, then pay. The whole path in one call. */
  async function delegate(providerId, prompt, onProgress = () => {}, opts = {}) {
    onProgress({ phase: 'quoting', provider: providerId });
    const q = await quote(providerId, prompt);
    onProgress({ phase: 'quoted', provider: providerId, units: q.estimate_units, price: q.price_tinybar });

    onProgress({ phase: 'dispatching', provider: providerId, quote: q.quote_id });
    const job = await dispatch(providerId, q.quote_id);
    const done = await waitFor(job.job_id, { ...opts, onProgress });
    onProgress({ phase: 'collecting', job: done.job_id, price: done.price_tinybar });
    const out = await collect(done.job_id);
    onProgress({ phase: 'paid', job: out.job_id, tx: out.tx_id, price: out.price_tinybar });
    return out;
  }

  return { findProviders, quote, quoteAll, dispatch, status, waitFor, collect, delegate, decisions, spend, accountId };
}
