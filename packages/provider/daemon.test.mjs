import { test } from 'node:test';
import assert from 'node:assert/strict';
import {
  runJob,
  validate,
  parseOptions,
  registrationBody,
  nextBackoff,
  connectURL,
  register,
  usage,
  INITIAL_BACKOFF,
  MAX_BACKOFF,
} from './daemon.mjs';
import { makeBackends } from './backends.mjs';

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

test('runJob reports an over-ceiling count without altering it', async () => {
  const [sent, send] = collect();
  const lines = [];
  await runJob(async () => ({ result: 'r', units: 5000 }), send, {
    job_id: 'job-1',
    prompt: 'p',
    max_units: 100,
  }, (m) => lines.push(m));

  // The daemon reports honestly and lets the registry clamp: silently trimming here would destroy
  // the only cross-call evidence of inflation.
  assert.equal(sent[1].units, 5000);
  assert.match(lines.join('\n'), /ceiling/);
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
