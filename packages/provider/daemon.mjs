#!/usr/bin/env node
// The provider daemon: registers with a registry, dials it, and serves delegated jobs.
//
// It never listens on a port. The connection is outbound and stays open, which is what lets a
// machine behind NAT sell. It also holds no private key: payments settle directly to the account
// id declared at registration, and the registry is never custodial, so there is nothing here to
// sign with and nothing to withdraw.
import { parseArgs } from 'node:util';
import { pathToFileURL } from 'node:url';
import { mkdtempSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { makeBackends } from './backends.mjs';

export const MAX_BACKOFF = 30_000;
export const INITIAL_BACKOFF = 1000;

export function parseOptions(argv = process.argv.slice(2), env = process.env) {
  const { values } = parseArgs({
    args: argv,
    options: {
      registry: { type: 'string', default: env.REGISTRY_URL ?? 'http://localhost:8080' },
      account: { type: 'string', default: env.PROVIDER_ACCOUNT_ID ?? '' },
      name: { type: 'string', default: 'Local Provider' },
      backend: { type: 'string', default: 'echo' },
      model: { type: 'string', default: '' },
      rate: { type: 'string', default: '1' }, // tinybar per unit
      capability: { type: 'string', default: 'text-generation' },
      'provider-id': { type: 'string', default: '' },
      tools: { type: 'string', default: 'none' },
      workdir: { type: 'string', default: '' },
      help: { type: 'boolean', default: false },
    },
  });
  return values;
}

export function usage(backendNames) {
  return `provider-daemon — sell inference through an Inference Exchange registry

  --account <0.0.x>     Hedera account id where payments land   (required)
  --registry <url>      registry base URL            (default http://localhost:8080)
  --backend <name>      ${backendNames.join(' | ')}  (default echo)
  --model <name>        backend-specific model name
  --rate <tinybar>      price per unit of work                   (default 1)
  --name <text>         display name in the listing
  --capability <name>   what is being sold          (default text-generation)
  --provider-id <id>    reconnect as an existing registration
  --tools none|all      whether a buyer's prompt may use tools    (default none)
  --workdir <path>      where backends run (default: a fresh temp directory)

No private key is needed or accepted: payments settle straight to --account.`;
}

/** Returns an error string if the options cannot be used, or null if they are fine. */
export function validate(opt, backends) {
  if (!opt.account) return 'a Hedera account id is required (--account)';
  const rate = Number(opt.rate);
  if (!Number.isFinite(rate) || rate <= 0) {
    return `--rate must be a positive number of tinybar, got ${opt.rate}`;
  }
  if (!backends[opt.backend]) {
    return `unknown backend ${opt.backend}; have: ${Object.keys(backends).join(', ')}`;
  }
  if (opt.tools !== undefined && !['none', 'all'].includes(opt.tools)) {
    return `--tools must be none or all, got ${opt.tools}`;
  }
  return null;
}

/**
 * The body sent to POST /providers.
 *
 * Everything under `declared` is provider-asserted and never verified. The key name is the
 * mechanism: it keeps self-reports visibly separate from settled facts wherever a listing is
 * rendered.
 */
export function registrationBody(opt) {
  return {
    account_id: opt.account,
    display_name: opt.name,
    capability: opt.capability,
    rate_per_unit: Number(opt.rate),
    declared: { backend: opt.backend, model: opt.model || null, host: 'self-hosted' },
  };
}

export function nextBackoff(current) {
  return Math.min(current * 2, MAX_BACKOFF);
}

/**
 * Run one job frame and answer it.
 *
 * Never throws and never stays silent. A job the backend could not do must come back as a `failed`
 * frame, because that is what leaves the buyer unbilled — a throw here would strand them waiting
 * for a result that is never coming, holding a job the registry believes is still running.
 */
export async function runJob(execute, send, frame, log = () => {}) {
  const started = Date.now();
  send({ type: 'accepted', job_id: frame.job_id });
  try {
    const { result, units, costUSD } = await execute(frame.prompt, frame.max_units);
    send({ type: 'result', job_id: frame.job_id, result: String(result ?? ''), units });
    const over =
      units > frame.max_units
        ? `  (over the buyer's ceiling of ${frame.max_units}; it will be clamped)`
        : '';
    // Cost is the provider's own concern, not the buyer's price, so it is logged here rather than
    // sent. A backend that knows what a job cost lets its operator see the margin per job.
    const cost = typeof costUSD === 'number' ? `  cost=$${costUSD.toFixed(6)}` : '';
    log(`  done ${Date.now() - started}ms  units=${units}${cost}${over}`);
  } catch (e) {
    const detail = String(e?.message ?? e).slice(0, 300);
    send({ type: 'failed', job_id: frame.job_id, error: detail });
    log(`  failed ${Date.now() - started}ms  ${detail.slice(0, 160)}`);
  }
}

/**
 * A registration attempt failed. `status` is the registry's HTTP status, or undefined when the
 * request never got an answer at all.
 */
export class RegistrationError extends Error {
  constructor(message, status) {
    super(message);
    this.name = 'RegistrationError';
    this.status = status;
  }
}

/**
 * Whether a registration failure is worth trying again.
 *
 * No answer, or a 5xx, means the registry is not up yet or is unwell — both are worth waiting out,
 * and starting the registry and a provider from the same script makes the first one routine. A 4xx
 * means this registration is wrong, and sending it again will keep being wrong: retrying would turn
 * a clear error into a silent loop.
 */
export function isRetryable(err) {
  if (!(err instanceof RegistrationError)) return true; // unrecognised: assume transient
  if (err.status === undefined) return true;
  return err.status >= 500;
}

export async function register(registryURL, opt, fetchFn = globalThis.fetch) {
  let res;
  try {
    res = await fetchFn(`${registryURL}/providers`, {
      method: 'POST',
      headers: { 'content-type': 'application/json' },
      body: JSON.stringify(registrationBody(opt)),
    });
  } catch (e) {
    throw new RegistrationError(`registry unreachable at ${registryURL}: ${e.message}`);
  }
  if (!res.ok) {
    throw new RegistrationError(
      `register failed ${res.status}: ${(await res.text()).slice(0, 200)}`,
      res.status,
    );
  }
  return (await res.json()).provider_id;
}

export function connectURL(registryURL, providerId) {
  return `${registryURL.replace(/^http/, 'ws')}/connect?provider_id=${providerId}`;
}

// --- entry point -----------------------------------------------------------

async function main() {
  const opt = parseOptions();
  const backends = makeBackends(opt);

  if (opt.help) {
    console.log(usage(Object.keys(backends)));
    return;
  }
  const problem = validate(opt, backends);
  if (problem) {
    console.error(problem + '\n\n' + usage(Object.keys(backends)));
    process.exit(1);
  }

  // Backends run somewhere with nothing of the provider's in it, rather than wherever the daemon
  // was launched from. With tools off this is belt and braces; with `--tools all` it is the only
  // thing standing between a buyer's prompt and the provider's files.
  if (!opt.workdir) {
    opt.workdir = mkdtempSync(join(tmpdir(), 'ix-provider-'));
  }
  if (opt.tools === 'all') {
    console.error(
      `⚠ --tools all: buyers' prompts may run tools in ${opt.workdir}. Only do this in a sandbox.`,
    );
  }

  const execute = backends[opt.backend];
  let providerId = opt['provider-id'];
  let backoff = INITIAL_BACKOFF;

  async function connect() {
    if (!providerId) {
      try {
        providerId = await register(opt.registry, opt);
        console.error(`registered as ${providerId}  (account ${opt.account}, ${opt.rate} tinybar/unit)`);
      } catch (e) {
        if (!isRetryable(e)) {
          console.error(`fatal: ${e.message}`);
          process.exit(1);
        }
        // The same backoff ladder the socket uses. A registry that is still starting up — HCS setup
        // alone adds seconds — is the common case when both are launched from one script.
        console.error(`could not register (${e.message}) — retrying in ${backoff}ms`);
        setTimeout(connect, backoff);
        backoff = nextBackoff(backoff);
        return;
      }
    }

    const url = connectURL(opt.registry, providerId);
    const ws = new WebSocket(url);
    const send = (obj) => {
      if (ws.readyState === WebSocket.OPEN) ws.send(JSON.stringify(obj));
    };

    ws.addEventListener('open', () => {
      backoff = INITIAL_BACKOFF;
      console.error(`online  → ${url}   backend=${opt.backend}`);
    });

    ws.addEventListener('message', (ev) => {
      let f;
      try {
        f = JSON.parse(ev.data);
      } catch {
        return;
      }
      if (f.type !== 'job') return;
      console.error(`job ${f.job_id}  max_units=${f.max_units}  "${String(f.prompt).slice(0, 60)}"`);
      // Deliberately not awaited: the read loop must stay responsive while work is in progress.
      runJob(execute, send, f, (m) => console.error(m));
    });

    ws.addEventListener('close', () => {
      console.error(`offline — reconnecting in ${backoff}ms`);
      setTimeout(connect, backoff);
      backoff = nextBackoff(backoff);
    });

    // 'close' always follows 'error' and owns the retry, so retrying here would double-schedule.
    ws.addEventListener('error', () => {});
  }

  await connect();
}

// Only run when executed directly, so the module can be imported and tested.
if (import.meta.url === pathToFileURL(process.argv[1] ?? '').href) {
  main().catch((e) => {
    console.error('fatal:', e.message);
    process.exit(1);
  });
}
