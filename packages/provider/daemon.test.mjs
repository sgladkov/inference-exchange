import { test } from 'node:test';
import assert from 'node:assert/strict';
import {
  runJob,
  runQuote,
  validate,
  parseOptions,
  registrationBody,
  nextBackoff,
  connectURL,
  register,
  isRetryable,
  RegistrationError,
  usage,
  INITIAL_BACKOFF,
  MAX_BACKOFF,
} from './daemon.mjs';
import { makeBackends, makeEstimators } from './backends.mjs';

const collect = () => {
  const sent = [];
  return [sent, (f) => sent.push(f)];
};

// --- runJob: the invariant that keeps a buyer unbilled ----------------------

test('runJob accepts then returns a result', async () => {
  const [sent, send] = collect();
  await runJob(async () => ({ result: 'the answer', units: 12 }), send, {
    job_id: 'job-1',
    prompt: 'p',
    max_units: 100,
  });

  assert.deepEqual(
    sent.map((f) => f.type),
    ['accepted', 'result'],
  );
  assert.equal(sent[1].result, 'the answer');
  assert.equal(sent[1].units, 12);
  assert.equal(sent[1].job_id, 'job-1');
});

test('runJob reports a backend failure instead of throwing', async () => {
  const [sent, send] = collect();
  // A throw escaping here would strand the buyer waiting on a job the registry thinks is running.
  await assert.doesNotReject(() =>
    runJob(
      async () => {
        throw new Error('the backend exploded');
      },
      send,
      { job_id: 'job-1', prompt: 'p', max_units: 100 },
    ),
  );

  assert.deepEqual(
    sent.map((f) => f.type),
    ['accepted', 'failed'],
  );
  assert.match(sent[1].error, /the backend exploded/);
  assert.equal(sent[1].job_id, 'job-1');
  assert.equal(sent[1].result, undefined, 'a failed job must not carry a result');
});

test('runJob answers even when the backend throws a non-Error', async () => {
  const [sent, send] = collect();
  await runJob(
    async () => {
      throw 'a bare string';
    },
    send,
    { job_id: 'job-1', prompt: 'p', max_units: 10 },
  );
  assert.equal(sent[1].type, 'failed');
  assert.match(sent[1].error, /a bare string/);
});

test('runJob always answers exactly one terminal frame', async () => {
  for (const execute of [
    async () => ({ result: 'ok', units: 1 }),
    async () => {
      throw new Error('nope');
    },
    async () => ({ result: undefined, units: 1 }),
  ]) {
    const [sent, send] = collect();
    await runJob(execute, send, { job_id: 'j', prompt: 'p', max_units: 10 });
    const terminal = sent.filter((f) => f.type === 'result' || f.type === 'failed');
    assert.equal(terminal.length, 1, `got ${JSON.stringify(sent)}`);
  }
});

test('runJob coerces a non-string result rather than sending a bad frame', async () => {
  const [sent, send] = collect();
  await runJob(async () => ({ result: undefined, units: 3 }), send, {
    job_id: 'job-1',
    prompt: 'p',
    max_units: 10,
  });
  assert.equal(sent[1].result, '', 'undefined should become an empty string, not vanish');
  assert.equal(typeof sent[1].result, 'string');
});

test('runJob reports an over-quote count without altering it', async () => {
  const [sent, send] = collect();
  const lines = [];
  await runJob(async () => ({ result: 'r', units: 5000 }), send, {
    job_id: 'job-1',
    prompt: 'p',
    max_units: 100,
  }, (m) => lines.push(m));

  // The daemon reports honestly and lets the registry clamp: silently trimming here would destroy
  // the only cross-call evidence of inflation. The overrun is now this provider's own loss, which
  // is the operator's cue that its estimator is wrong.
  assert.equal(sent[1].units, 5000);
  assert.match(lines.join('\n'), /absorb/);
});

// --- runQuote: pricing without committing to anything ----------------------

test('runQuote answers with an integer estimate against the quote id', async () => {
  const [sent, send] = collect();
  await runQuote(async (prompt) => prompt.length * 1.5, send, {
    quote_id: 'q-1',
    prompt: 'abcd',
  });

  assert.deepEqual(sent, [{ type: 'quoted', quote_id: 'q-1', units: 6 }]);
});

test('runQuote rounds up, because a fractional unit is still a unit we pay for', async () => {
  const [sent, send] = collect();
  await runQuote(async () => 10.01, send, { quote_id: 'q-1', prompt: 'p' });
  assert.equal(sent[0].units, 11);
});

test('a thrown estimator is a decline, carrying its reason', async () => {
  // This is how a backend refuses work: the estimator throws, and the registry hears "no" rather
  // than "broken". Providers get to be picky without it counting against them.
  const [sent, send] = collect();
  await assert.doesNotReject(() =>
    runQuote(
      async () => {
        throw new Error('prompt exceeds my context window');
      },
      send,
      { quote_id: 'q-1', prompt: 'x'.repeat(9_000_000) },
    ),
  );

  assert.equal(sent.length, 1);
  assert.equal(sent[0].type, 'declined');
  assert.equal(sent[0].quote_id, 'q-1');
  assert.match(sent[0].error, /exceeds my context window/);
  assert.equal(sent[0].units, undefined, 'a decline must not carry a price');
});

test('a nonsense estimate is a decline rather than a free job', async () => {
  // A zero or NaN estimate would price real work at nothing. Refusing is the safe reading: the
  // buyer can quote elsewhere, and nobody works for free by arithmetic accident.
  for (const bad of [0, -5, NaN, Infinity, undefined, null, 'lots']) {
    const [sent, send] = collect();
    await runQuote(async () => bad, send, { quote_id: 'q-1', prompt: 'p' });
    assert.equal(sent[0].type, 'declined', `estimate ${String(bad)} was quoted, not declined`);
  }
});

test('runQuote always answers exactly one frame', async () => {
  // Silence would park the registry's quote call until it times out, and the buyer with it.
  for (const estimate of [
    async () => 42,
    async () => {
      throw new Error('no');
    },
    async () => {
      throw 'a bare string';
    },
    async () => 0,
  ]) {
    const [sent, send] = collect();
    await runQuote(estimate, send, { quote_id: 'q-1', prompt: 'p' });
    assert.equal(sent.length, 1, `got ${JSON.stringify(sent)}`);
    assert.ok(['quoted', 'declined'].includes(sent[0].type));
  }
});

// --- estimators ------------------------------------------------------------

test('every backend has an estimator, or it cannot be sold from', () => {
  const backends = Object.keys(makeBackends());
  const estimators = makeEstimators();
  for (const name of backends) {
    assert.equal(typeof estimators[name], 'function', `${name} has no estimator`);
  }
});

test('a longer prompt costs more than a short one, from the same backend', async () => {
  const estimators = makeEstimators({ backend: 'claude-code', model: 'sonnet' });
  const short = await estimators['claude-code']('hi');
  const long = await estimators['claude-code']('x'.repeat(40_000));
  assert.ok(long > short, `${long} should exceed ${short}`);
});

test('the claude-code estimate clears the measured floor even for a two-character prompt', () => {
  // The floor is the whole reason a static per-unit rate could not work: a trivial prompt still
  // costs ~14,000 tokens of agent context, and pricing it by prompt length alone sells at a loss.
  const estimators = makeEstimators({ backend: 'claude-code', model: 'opus' });
  assert.ok(estimators['claude-code']('hi') > 14_196, 'opus quote fell below its own floor');
});

test('an unknown model is priced as the most expensive one', () => {
  // Defaulting low would quote below cost for the flagship. Being wrong in the provider's favour
  // is a lost sale; being wrong the other way is unpaid work.
  const guessed = makeEstimators({ model: 'some-future-model' })['claude-code']('hi');
  const opus = makeEstimators({ model: 'opus' })['claude-code']('hi');
  assert.equal(guessed, opus);
});

// --- options ---------------------------------------------------------------

test('validate requires an account', () => {
  const backends = makeBackends();
  assert.match(validate({ account: '', rate: '1', backend: 'echo' }, backends), /account/);
  assert.equal(validate({ account: '0.0.1', rate: '1', backend: 'echo' }, backends), null);
});

test('validate rejects a non-positive or unparseable rate', () => {
  const backends = makeBackends();
  for (const rate of ['0', '-1', 'abc', '']) {
    assert.match(
      validate({ account: '0.0.1', rate, backend: 'echo' }, backends),
      /rate/,
      `rate ${JSON.stringify(rate)} should be refused`,
    );
  }
});

test('validate rejects a --tools value that is neither none nor all', () => {
  const backends = makeBackends();
  const base = { account: '0.0.1', rate: '1', backend: 'echo' };
  assert.equal(validate({ ...base, tools: 'none' }, backends), null);
  assert.equal(validate({ ...base, tools: 'all' }, backends), null);
  assert.match(validate({ ...base, tools: 'some' }, backends), /--tools/);
  // Anything unrecognised must not quietly fall through to the permissive branch.
  assert.match(validate({ ...base, tools: 'ALL' }, backends), /--tools/);
});

test('validate rejects an unknown backend and names the real ones', () => {
  const backends = makeBackends();
  const msg = validate({ account: '0.0.1', rate: '1', backend: 'telepathy' }, backends);
  assert.match(msg, /telepathy/);
  assert.match(msg, /echo/, 'the error should list what is actually available');
});

test('parseOptions reads env fallbacks', () => {
  const opt = parseOptions([], {
    REGISTRY_URL: 'http://registry.test:9000',
    PROVIDER_ACCOUNT_ID: '0.0.4242',
  });
  assert.equal(opt.registry, 'http://registry.test:9000');
  assert.equal(opt.account, '0.0.4242');
  assert.equal(opt.backend, 'echo');
});

test('parseOptions lets flags win over env', () => {
  const opt = parseOptions(['--account', '0.0.1', '--backend', 'ollama'], {
    PROVIDER_ACCOUNT_ID: '0.0.9999',
  });
  assert.equal(opt.account, '0.0.1');
  assert.equal(opt.backend, 'ollama');
});

test('usage names every available backend', () => {
  const text = usage(Object.keys(makeBackends()));
  for (const name of ['echo', 'claude-code', 'anthropic', 'ollama']) {
    assert.match(text, new RegExp(name));
  }
});

// --- registration ----------------------------------------------------------

test('registrationBody carries no key material', () => {
  const body = registrationBody({
    account: '0.0.5005',
    name: 'Echo Tier',
    capability: 'text-generation',
    rate: '3',
    backend: 'echo',
    model: '',
  });
  assert.equal(body.account_id, '0.0.5005');
  assert.equal(body.rate_per_unit, 3, 'rate must be sent as a number, not a string');

  // The daemon has no private key at all; this asserts none ever appears in the payload.
  const json = JSON.stringify(body).toLowerCase();
  for (const forbidden of ['privatekey', 'private_key', 'secret', 'mnemonic', 'seed']) {
    assert.ok(!json.includes(forbidden), `registration payload contains ${forbidden}`);
  }
});

test('registrationBody keeps self-reports under declared', () => {
  const body = registrationBody({
    account: '0.0.1',
    rate: '1',
    backend: 'claude-code',
    model: 'claude-opus-5',
    name: 'n',
    capability: 'text-generation',
  });
  assert.equal(body.declared.backend, 'claude-code');
  assert.equal(body.declared.model, 'claude-opus-5');
  // Unverifiable claims must not sit at the top level next to settled facts.
  assert.equal(body.backend, undefined);
  assert.equal(body.model, undefined);
});

test('register returns the provider id and fails loudly otherwise', async () => {
  const okFetch = async () => ({ ok: true, status: 201, json: async () => ({ provider_id: 'prov-1' }) });
  assert.equal(await register('http://r', { account: '0.0.1', rate: '1' }, okFetch), 'prov-1');

  const badFetch = async () => ({ ok: false, status: 400, text: async () => 'bad account' });
  await assert.rejects(
    () => register('http://r', { account: '', rate: '1' }, badFetch),
    /400.*bad account/s,
  );
});

// --- registration failures: which are worth waiting out --------------------

test('register reports the status so the caller can decide about retrying', async () => {
  const rejecting = async () => ({ ok: false, status: 400, text: async () => 'bad account' });
  await assert.rejects(() => register('http://r', { rate: '1' }, rejecting), (e) => {
    assert.ok(e instanceof RegistrationError, `got ${e.name}`);
    assert.equal(e.status, 400);
    return true;
  });
});

// A registry that is not listening yet gives no answer at all. This is the case that killed the
// daemon before: it must be distinguishable from a rejection.
test('an unreachable registry has no status, and names the URL', async () => {
  const unreachable = async () => {
    throw new TypeError('fetch failed');
  };
  await assert.rejects(() => register('http://localhost:9', { rate: '1' }, unreachable), (e) => {
    assert.ok(e instanceof RegistrationError, `got ${e.name}`);
    assert.equal(e.status, undefined, 'a transport failure must not invent a status');
    assert.match(e.message, /localhost:9/, 'the operator should be told which registry');
    return true;
  });
});

test('isRetryable waits out a registry that is down, and gives up on a bad registration', () => {
  // Not yet listening, or unwell: worth waiting out. Launching the registry and a provider from one
  // script makes the first of these routine.
  assert.equal(isRetryable(new RegistrationError('unreachable')), true);
  assert.equal(isRetryable(new RegistrationError('server error', 500)), true);
  assert.equal(isRetryable(new RegistrationError('gateway', 503)), true);

  // Our request is wrong and will stay wrong; looping would hide a clear error.
  assert.equal(isRetryable(new RegistrationError('bad account', 400)), false);
  assert.equal(isRetryable(new RegistrationError('not found', 404)), false);
  assert.equal(isRetryable(new RegistrationError('conflict', 409)), false);

  // Anything unrecognised is treated as transient rather than fatal: a daemon that keeps trying is
  // recoverable, one that exited is not.
  assert.equal(isRetryable(new TypeError('fetch failed')), true);
});

// --- connection ------------------------------------------------------------

test('connectURL switches scheme and carries the provider id', () => {
  assert.equal(
    connectURL('http://localhost:8080', 'prov-1'),
    'ws://localhost:8080/connect?provider_id=prov-1',
  );
  assert.equal(
    connectURL('https://exchange.example', 'prov-2'),
    'wss://exchange.example/connect?provider_id=prov-2',
  );
});

// Registration and reconnection share one ladder, so a registry that appears six seconds late is
// picked up within a couple of attempts rather than needing a restart.
test('the registration retry ladder reaches a late registry quickly', () => {
  let b = INITIAL_BACKOFF;
  let waited = 0;
  let attempts = 1;
  while (waited < 6000) {
    waited += b;
    b = nextBackoff(b);
    attempts++;
  }
  assert.ok(attempts <= 4, `took ${attempts} attempts to cover 6s`);
});

test('backoff doubles and then holds at the ceiling', () => {
  let b = INITIAL_BACKOFF;
  const seen = [b];
  for (let i = 0; i < 10; i++) {
    b = nextBackoff(b);
    seen.push(b);
  }
  assert.deepEqual(seen.slice(0, 4), [1000, 2000, 4000, 8000]);
  assert.equal(b, MAX_BACKOFF, 'backoff must stop growing');
  assert.ok(seen.every((v) => v <= MAX_BACKOFF));
});

test('runJob logs what a job cost the provider, without sending it', async () => {
  const [sent, send] = collect();
  const lines = [];
  await runJob(
    async () => ({ result: 'r', units: 14200, costUSD: 0.00825 }),
    send,
    { job_id: 'job-1', prompt: 'p', max_units: 20000 },
    (m) => lines.push(m),
  );
  assert.match(lines.join('\n'), /cost=\$0\.008250/);
  // The buyer pays for units, not for the provider's cost — the two must not be conflated on the
  // wire, or a warm cache would quietly change the price.
  assert.equal(sent[1].costUSD, undefined);
  assert.equal(sent[1].units, 14200);
});
