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
 * Separated from the subprocess call so the parsing is testable without spawning anything, and so
 * an output-shape change is a one-line fix in a covered function.
 */
export function parseClaudeCodeOutput(stdout, prompt) {
  let parsed;
  try {
    parsed = JSON.parse(stdout);
  } catch {
    parsed = null;
  }
  const result = parsed?.result ?? parsed?.text ?? stdout;
  const u = parsed?.usage ?? {};
  const reported = (u.input_tokens ?? 0) + (u.output_tokens ?? 0);
  // Prefer the session's own accounting; fall back to an estimate so a usage-less run still
  // produces a billable number rather than zero.
  return { result: String(result), units: reported || estimateUnits(prompt, result) };
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
    async 'claude-code'(prompt) {
      const args = ['-p', prompt, '--output-format', 'json'];
      if (opt.model) args.push('--model', opt.model);
      return parseClaudeCodeOutput(await run('claude', args), prompt);
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

function spawnAndCollect(cmd, args) {
  return new Promise((resolve, reject) => {
    const p = spawn(cmd, args, { stdio: ['ignore', 'pipe', 'pipe'] });
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
