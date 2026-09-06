import { test } from 'node:test';
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import { makeBackends, parseClaudeCodeOutput, claudeCodeUnits, estimateUnits } from './backends.mjs';

// Two real `claude -p --output-format json` captures: one with a cold cache, one warm. They are
// whole payloads, not trimmed to the fields read today, because trimming to assumed fields is what
// produced the bug these pin.
const capture = (name) =>
  readFileSync(new URL(`./fixtures/claude-code-${name}.json`, import.meta.url), 'utf8');

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

// --- the bug these fixtures exist for ------------------------------------

// Counting only input+output reported 10 units for an invocation that consumed 14,200 tokens. It
// never tripped a fallback, because 10 is a plausible-looking number.
test('units count every token an invocation consumed', () => {
  for (const state of ['cold', 'warm']) {
    const raw = capture(state);
    const out = parseClaudeCodeOutput(raw, 'Reply with exactly: hello from the provider');
    assert.equal(out.units, 14200, `${state} cache`);

    const u = JSON.parse(raw).usage;
    assert.ok(
      out.units > (u.input_tokens + u.output_tokens) * 100,
      `${state}: ${out.units} units is close to input+output (${u.input_tokens + u.output_tokens}); ` +
        'the cache fields are being dropped again',
    );
  }
});

// The decisive measurement: identical work, identical token total, eightfold cost difference driven
// only by cache warmth. Billing tokens keeps a buyer's ceiling stable; billing cost would not.
test('token count is stable across cache states even though cost is not', () => {
  const cold = parseClaudeCodeOutput(capture('cold'), 'p');
  const warm = parseClaudeCodeOutput(capture('warm'), 'p');

  assert.equal(cold.units, warm.units, 'the same work should bill the same');
  assert.ok(
    cold.costUSD > warm.costUSD * 5,
    `cost should differ sharply by cache state: ${cold.costUSD} vs ${warm.costUSD}`,
  );
});

test('cost is reported alongside the units, for the provider to watch its margin', () => {
  const out = parseClaudeCodeOutput(capture('warm'), 'p');
  assert.equal(typeof out.costUSD, 'number');
  assert.ok(out.costUSD > 0);
});

test('parseClaudeCodeOutput reads the result off a real capture', () => {
  const out = parseClaudeCodeOutput(capture('cold'), 'p');
  assert.equal(out.result, 'hello from the provider');
});

test('claudeCodeUnits tolerates a payload with no usage at all', () => {
  assert.equal(claudeCodeUnits(null), 0);
  assert.equal(claudeCodeUnits({}), 0);
  assert.equal(claudeCodeUnits({ usage: {} }), 0);
  // Partial usage should still sum what is there rather than returning nothing.
  assert.equal(claudeCodeUnits({ usage: { input_tokens: 5, cache_read_input_tokens: 7 } }), 12);
});

// --- runs that exit 0 without succeeding ---------------------------------

// The CLI can exit 0 and still have failed. Selling that as a completed job charges a buyer for
// nothing, so these are raised and reach the buyer as a `failed` frame.
test('an is_error payload is a failure, not a result', () => {
  const raw = JSON.stringify({ is_error: true, subtype: 'error_during_execution', result: 'boom' });
  assert.throws(() => parseClaudeCodeOutput(raw, 'p'), /did not complete.*boom/s);
});

test('a non-success subtype is a failure', () => {
  const raw = JSON.stringify({ is_error: false, subtype: 'error_max_turns', result: '' });
  assert.throws(() => parseClaudeCodeOutput(raw, 'p'), /did not complete/);
});

// A daemon runs unattended, so a permission prompt it cannot answer produces an empty-looking
// success. The operator needs to be told that is what happened.
test('permission denials are a failure that names the cause', () => {
  const raw = JSON.stringify({
    subtype: 'success',
    result: '',
    permission_denials: [{ tool_name: 'Bash' }],
  });
  assert.throws(() => parseClaudeCodeOutput(raw, 'p'), /denied permissions.*unattended/s);
});

test('a real capture has none of those failure markers', () => {
  for (const state of ['cold', 'warm']) {
    assert.doesNotThrow(() => parseClaudeCodeOutput(capture(state), 'p'));
  }
});

test('parseClaudeCodeOutput falls back when usage is missing entirely', () => {
  const out = parseClaudeCodeOutput(JSON.stringify({ subtype: 'success', result: 'the answer' }), 'the prompt');
  assert.equal(out.result, 'the answer');
  assert.ok(out.units > 0, 'a usage-less run must still be billable');
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

// --- tools are off unless asked for --------------------------------------

// Selling inference is not selling code execution on your own machine. It is also what makes the
// backend work unattended: a prompt that reaches for a tool hits a permission nobody can grant and
// comes back as denials and an empty result. Observed on a real run before this was added.
test('claude-code disallows tools by default', async () => {
  let seen;
  const { 'claude-code': claude } = makeBackends(
    {},
    { run: async (_cmd, args) => ((seen = args), JSON.stringify({ result: 'r' })) },
  );
  await claude('prompt');
  const i = seen.indexOf('--allowedTools');
  assert.ok(i >= 0, `no --allowedTools in ${JSON.stringify(seen)}`);
  assert.equal(seen[i + 1], '', 'the allowed set must be empty, not merely present');
  assert.ok(!seen.includes('--dangerously-skip-permissions'));
});

test('claude-code enables tools only on an explicit opt-in', async () => {
  let seen;
  const { 'claude-code': claude } = makeBackends(
    { tools: 'all' },
    { run: async (_cmd, args) => ((seen = args), JSON.stringify({ result: 'r' })) },
  );
  await claude('prompt');
  assert.ok(seen.includes('--dangerously-skip-permissions'));
  assert.ok(!seen.includes('--allowedTools'));
});

test('claude-code runs in the configured working directory', async () => {
  let opts;
  const { 'claude-code': claude } = makeBackends(
    { workdir: '/tmp/ix-somewhere' },
    { run: async (_cmd, _args, o) => ((opts = o), JSON.stringify({ result: 'r' })) },
  );
  await claude('prompt');
  assert.equal(opts?.cwd, '/tmp/ix-somewhere', 'a backend must not run wherever the daemon was started');
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

test('echo sleeps on demand so a long job can be demonstrated', async () => {
  const { echo } = makeBackends();
  const started = Date.now();
  const out = await echo('sleep:1');
  const elapsed = Date.now() - started;
  assert.ok(elapsed >= 1000, `took ${elapsed}ms, expected at least 1000`);
  assert.ok(out.units > 0);

  // Only an exact `sleep:<n>` triggers it; a prompt that merely mentions sleep must not.
  const t2 = Date.now();
  await echo('please tell me about sleep:9999 in birds');
  assert.ok(Date.now() - t2 < 1000, 'a prompt mentioning sleep should not actually sleep');
});
