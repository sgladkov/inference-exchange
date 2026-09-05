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

  /** Price a call before committing to it, from the provider's advertised rate. */
  async function quote(providerId, maxUnits) {
    const res = await fetchFn(`${registry}/providers/${providerId}`);
    if (!res.ok) throw new Error(`quote ${res.status}`);
    const p = await res.json();
    return {
      provider_id: p.provider_id,
      rate_per_unit: p.rate_per_unit,
      max_units: maxUnits,
      // The ceiling, not a prediction. Actual usage is priced at collect and can be lower.
      max_price_tinybar: p.rate_per_unit * maxUnits,
      pay_to: p.pay_to,
      declared: p.declared,
    };
  }

  /**
   * Dispatch a job. Returns as soon as the registry accepts it, with a job id — not a result.
   *
   * Free, and asynchronous: no payment is taken here and no Hedera transaction is in flight while
   * the work runs, so the job may take minutes without anything expiring. Holding the id from the
   * outset is what lets a caller report progress and come back after a crash.
   */
  async function dispatch(providerId, prompt, maxUnits) {
    const res = await fetchFn(`${registry}/p/${providerId}/job`, {
      method: 'POST',
      headers,
      body: JSON.stringify({ prompt, max_units: maxUnits }),
    });
    const body = await res.json().catch(() => ({}));
    if (res.status === 403) throw classify(body.error ?? 'denied at dispatch');
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

  /** Dispatch, wait for the work, then pay. The whole path in one call. */
  async function delegate(providerId, prompt, maxUnits, onProgress = () => {}, opts = {}) {
    onProgress({ phase: 'dispatching', provider: providerId });
    const job = await dispatch(providerId, prompt, maxUnits);
    const done = await waitFor(job.job_id, { ...opts, onProgress });
    onProgress({ phase: 'collecting', job: done.job_id, price: done.price_tinybar });
    const out = await collect(done.job_id);
    onProgress({ phase: 'paid', job: out.job_id, tx: out.tx_id, price: out.price_tinybar });
    return out;
  }

  return { findProviders, quote, dispatch, status, waitFor, collect, delegate, accountId };
}
