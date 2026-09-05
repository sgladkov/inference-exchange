import { test } from 'node:test';
import assert from 'node:assert/strict';
import { makeBackends, parseClaudeCodeOutput, estimateUnits } from './backends.mjs';

// A backend that reports zero units would be free work; one that throws must reach the daemon as a
// failure so the buyer goes unbilled. Both are checked for every backend below.

const ok = (body) => ({ ok: true, status: 200, json: async () => body });
const notOk = (status, text) => ({ ok: false, status, text: async () => text });

test('every backend returns the { result, units } contract', async () => {
  const backends = makeBackends(
    {},
    {
      run: async () => JSON.stringify({ result: 'from claude', usage: { input_tokens: 3, output_tokens: 4 } }),
      fetch: async (url) =>
        String(url).includes('anthropic')
          ? ok({ content: [{ text: 'hi' }], usage: { input_tokens: 1, output_tokens: 2 } })
          : ok({ response: 'hi', prompt_eval_count: 1, eval_count: 2 }),
      env: { ANTHROPIC_API_KEY: 'sk-test' },
    },
  );

  for (const [name, fn] of Object.entries(backends)) {
    const out = await fn('a prompt', 100);
    assert.equal(typeof out.result, 'string', `${name} result`);
    assert.equal(typeof out.units, 'number', `${name} units`);
    assert.ok(out.units > 0, `${name} reported ${out.units} units; zero would be free work`);
    assert.ok(Number.isInteger(out.units), `${name} units must be whole: ${out.units}`);
  }
});

test('echo is deterministic and scales with prompt length', async () => {
  const { echo } = makeBackends();
  const short = await echo('hi');
  const long = await echo('x'.repeat(400));
  assert.ok(short.units < long.units, 'units should grow with the prompt');
  assert.ok(short.result.includes('hi'));
  assert.deepEqual(await echo('hi'), short, 'echo should be deterministic');
});

test('estimateUnits never returns zero', () => {
  assert.equal(estimateUnits(''), 1);
  assert.equal(estimateUnits(null, undefined), 1);
  assert.ok(estimateUnits('x'.repeat(400)) > 1);
});

test('parseClaudeCodeOutput prefers the session usage', () => {
  const out = parseClaudeCodeOutput(
    JSON.stringify({ result: 'the answer', usage: { input_tokens: 10, output_tokens: 5 } }),
    'the prompt',
  );
  assert.equal(out.result, 'the answer');
  assert.equal(out.units, 15);
});

test('parseClaudeCodeOutput falls back when usage is missing or zero', () => {
  // A usage-less run must still produce a billable number rather than zero.
  const noUsage = parseClaudeCodeOutput(JSON.stringify({ result: 'the answer' }), 'the prompt');
  assert.ok(noUsage.units > 0);

  const zeroUsage = parseClaudeCodeOutput(
    JSON.stringify({ result: 'a', usage: { input_tokens: 0, output_tokens: 0 } }),
    'p',
  );
  assert.ok(zeroUsage.units > 0);
});

test('parseClaudeCodeOutput survives non-JSON output', () => {
  // If the CLI ever prints plain text, the work still happened and must still be sellable.
  const out = parseClaudeCodeOutput('just some text, not json at all', 'the prompt');
  assert.equal(out.result, 'just some text, not json at all');
  assert.ok(out.units > 0);
});

test('claude-code passes the model through when set', async () => {
  let seen;
  const { 'claude-code': claude } = makeBackends(
    { model: 'claude-opus-5' },
    { run: async (_cmd, args) => ((seen = args), JSON.stringify({ result: 'r' })) },
  );
  await claude('prompt');
  assert.ok(seen.includes('--model'));
  assert.ok(seen.includes('claude-opus-5'));
  assert.ok(seen.includes('--output-format') && seen.includes('json'));
});

test('claude-code omits --model when unset, rather than sending an empty one', async () => {
  let seen;
  const { 'claude-code': claude } = makeBackends(
    {},
    { run: async (_cmd, args) => ((seen = args), JSON.stringify({ result: 'r' })) },
  );
  await claude('prompt');
  assert.ok(!seen.includes('--model'));
});

test('anthropic refuses without a key rather than calling out', async () => {
  let called = false;
  const { anthropic } = makeBackends({}, { fetch: async () => ((called = true), ok({})), env: {} });
  await assert.rejects(() => anthropic('p', 100), /ANTHROPIC_API_KEY/);
  assert.equal(called, false, 'it should not have hit the network without a key');
});

test('anthropic bounds max_tokens by the buyer ceiling', async () => {
  let body;
  const { anthropic } = makeBackends(
    {},
    {
      fetch: async (_url, init) => {
        body = JSON.parse(init.body);
        return ok({ content: [{ text: 'hi' }], usage: { input_tokens: 1, output_tokens: 1 } });
      },
      env: { ANTHROPIC_API_KEY: 'sk-test' },
    },
  );

  await anthropic('p', 50);
  assert.equal(body.max_tokens, 256, 'a tiny ceiling should still leave a usable floor');

  await anthropic('p', 100000);
  assert.equal(body.max_tokens, 4096, 'a huge ceiling should be capped');
});

test('anthropic surfaces an API error as a failure', async () => {
  const { anthropic } = makeBackends(
    {},
    { fetch: async () => notOk(429, 'rate limited'), env: { ANTHROPIC_API_KEY: 'sk-test' } },
  );
  await assert.rejects(() => anthropic('p', 100), /429/);
});

test('ollama sends the configured model and reads its token counts', async () => {
  let url;
  let body;
  const { ollama } = makeBackends(
    { model: 'mistral' },
    {
      fetch: async (u, init) => {
        url = String(u);
        body = JSON.parse(init.body);
        return ok({ response: 'hello', prompt_eval_count: 7, eval_count: 8 });
      },
      env: { OLLAMA_URL: 'http://ollama.test:11434' },
    },
  );

  const out = await ollama('p');
  assert.ok(url.startsWith('http://ollama.test:11434'), url);
  assert.equal(body.model, 'mistral');
  assert.equal(body.stream, false, 'streaming would break the single-result contract');
  assert.equal(out.result, 'hello');
  assert.equal(out.units, 15);
});

test('ollama surfaces a transport error as a failure', async () => {
  const { ollama } = makeBackends({}, { fetch: async () => notOk(500, 'boom'), env: {} });
  await assert.rejects(() => ollama('p'), /ollama 500/);
});
