// Backend executors: the work a provider actually performs.
//
// Each takes (prompt, maxUnits) and returns { result, units }. `units` is what the provider
// reports it used; the registry clamps it to the buyer's declared ceiling before pricing, so an
// inflated count costs the provider its reputation rather than costing the buyer money.
//
// Adding a backend is the only thing that changes when a provider sells something new — the
// registration, wire protocol and payment path are identical regardless.
import { spawn } from 'node:child_process';

/** Rough token estimate for backends that do not report usage of their own. */
export function estimateUnits(...texts) {
  const chars = texts.reduce((n, t) => n + String(t ?? '').length, 0);
  return Math.max(1, Math.ceil(chars / 4));
}

/**
 * Pull the result and usage out of `claude -p --output-format json`.
 *
 * Separated from the subprocess call so the parsing is testable without spawning anything, and
 * pinned by fixtures/ — two real captures rather than an assumed shape. It was an assumed shape
 * that produced the bug this function exists in its current form to prevent.
 *
 * **Units are every token the invocation consumed, not just input plus output.** A trivial prompt
 * measured 2 input and 8 output tokens against 6,046 cache-creation and 8,144 cache-read tokens:
 * counting only the first two under-reports by roughly 1400x, and does so silently, because a
 * plausible small number never trips a fallback.
 *
 * Cost is reported but deliberately not billed on. Across the two captures the token total is
 * identical (14,200) while cost differs eightfold, purely by cache warmth — something the buyer can
 * neither see nor control. Billing tokens keeps a buyer's ceiling meaning the same thing every time
 * and leaves cache variance with the provider, which is the party that controls it.
 */
export function parseClaudeCodeOutput(stdout, prompt) {
  let parsed;
  try {
    parsed = JSON.parse(stdout);
  } catch {
    parsed = null;
  }

  // A run can exit 0 and still have failed. Selling that as a completed job would charge a buyer
  // for nothing, so it is raised and reaches them as a `failed` frame instead.
  if (parsed?.is_error === true || (parsed?.subtype && parsed.subtype !== 'success')) {
    const why = parsed.result || parsed.subtype || 'claude reported an error';
    throw new Error(`claude did not complete: ${String(why).slice(0, 200)}`);
  }
  if (Array.isArray(parsed?.permission_denials) && parsed.permission_denials.length > 0) {
    throw new Error(
      `claude was denied permissions it needed (${parsed.permission_denials.length}); ` +
        'the daemon likely needs them pre-granted to run unattended',
    );
  }

  const result = parsed?.result ?? parsed?.text ?? stdout;
  return {
    result: String(result),
    units: claudeCodeUnits(parsed) || estimateUnits(prompt, result),
    costUSD: parsed?.total_cost_usd,
  };
}

/**
 * Every token an invocation consumed.
 *
 * Cache-read tokens are cheaper than fresh input, so this slightly over-states cost — which is the
 * safe direction. Under-counting bills a provider's work at a fraction of what it cost them.
 */
export function claudeCodeUnits(parsed) {
  const u = parsed?.usage;
  if (!u) return 0;
  return (
    (u.input_tokens ?? 0) +
    (u.output_tokens ?? 0) +
    (u.cache_creation_input_tokens ?? 0) +
    (u.cache_read_input_tokens ?? 0)
  );
}

export function makeBackends(opt = {}, deps = {}) {
  const run = deps.run ?? spawnAndCollect;
  const fetchFn = deps.fetch ?? globalThis.fetch;
  const env = deps.env ?? process.env;

  return {
    // Deterministic, free, no external calls. The default so a demo can be wired up before any
    // model credentials exist.
    //
    // A prompt of the form `sleep:<seconds>` takes that long before answering. That exists so the
    // central claim of the design — that a job outlasting Hedera's 180-second transaction validity
    // window still settles — can be demonstrated without waiting on a real model to be slow.
    async echo(prompt) {
      const sleep = /^sleep:(\d+)$/.exec(prompt.trim());
      await new Promise((r) => setTimeout(r, sleep ? Number(sleep[1]) * 1000 : 300));
      return {
        result: `echo(${prompt.length} chars): ${prompt.slice(0, 400)}`,
        units: estimateUnits(prompt),
      };
    },

    // A local Claude Code instance, run headless. This is the flagship: one agent paying another
    // agent for work, with real HBAR moving between them.
    //
    // Note the floor. A two-token prompt still consumed 14,200 tokens, almost all of it Claude
    // Code's own context, so every job carries a large fixed cost regardless of how small the task
    // is. Providers should price with that in mind; tiny delegations are uneconomic here.
    async 'claude-code'(prompt) {
      const args = ['-p', prompt, '--output-format', 'json'];
      if (opt.model) args.push('--model', opt.model);

      // Tools are off unless a provider knowingly turns them on. Selling inference is not the same
      // as selling code execution on your own machine: with tools enabled, an anonymous buyer's
      // prompt can read files and run commands wherever the daemon happens to be.
      //
      // It is also what makes the backend work at all unattended. A prompt that reaches for a tool
      // hits a permission prompt nobody is there to answer, and comes back as five denials and an
      // empty result rather than an answer.
      if (opt.tools === 'all') {
        args.push('--dangerously-skip-permissions');
      } else {
        args.push('--allowedTools', '');
      }
      return parseClaudeCodeOutput(await run('claude', args, { cwd: opt.workdir }), prompt);
    },

    async anthropic(prompt, maxUnits) {
      const key = env.ANTHROPIC_API_KEY;
      if (!key) throw new Error('ANTHROPIC_API_KEY is not set');
      const res = await fetchFn('https://api.anthropic.com/v1/messages', {
        method: 'POST',
        headers: {
          'content-type': 'application/json',
          'x-api-key': key,
          'anthropic-version': '2023-06-01',
        },
        body: JSON.stringify({
          model: opt.model || 'claude-sonnet-5',
          max_tokens: Math.min(4096, Math.max(256, maxUnits ?? 1024)),
          messages: [{ role: 'user', content: prompt }],
        }),
      });
      if (!res.ok) {
        throw new Error(`anthropic ${res.status}: ${(await res.text()).slice(0, 200)}`);
      }
      const j = await res.json();
      const result = (j.content ?? []).map((c) => c.text ?? '').join('');
      const units = (j.usage?.input_tokens ?? 0) + (j.usage?.output_tokens ?? 0);
      return { result, units: units || estimateUnits(prompt, result) };
    },

    async ollama(prompt) {
      const base = env.OLLAMA_URL ?? 'http://localhost:11434';
      const res = await fetchFn(`${base}/api/generate`, {
        method: 'POST',
        headers: { 'content-type': 'application/json' },
        body: JSON.stringify({ model: opt.model || 'llama3', prompt, stream: false }),
      });
      if (!res.ok) throw new Error(`ollama ${res.status}`);
      const j = await res.json();
      const result = j.response ?? '';
      const units = (j.prompt_eval_count ?? 0) + (j.eval_count ?? 0);
      return { result, units: units || estimateUnits(prompt, result) };
    },
  };
}

function spawnAndCollect(cmd, args, { cwd } = {}) {
  return new Promise((resolve, reject) => {
    const p = spawn(cmd, args, { stdio: ['ignore', 'pipe', 'pipe'], cwd });
    let out = '';
    let err = '';
    p.stdout.on('data', (d) => {
      out += d;
    });
    p.stderr.on('data', (d) => {
      err += d;
    });
    p.on('error', reject);
    p.on('close', (code) =>
      code === 0 ? resolve(out) : reject(new Error(`${cmd} exited ${code}: ${err.slice(0, 300)}`)),
    );
  });
}
