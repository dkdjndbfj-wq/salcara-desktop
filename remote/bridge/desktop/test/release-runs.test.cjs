const { test } = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const { verifyReleaseRuns, reserveReleaseTag } = require('../scripts/verify-release-runs.cjs');

const input = {
  repository: 'dkdjndbfj-wq/salcara-desktop', sourceSHA: 'a'.repeat(40),
  buildRun: '1234567', sourceCheckRun: '1234568',
};

function runFor(endpoint) {
  const id = endpoint.split('/').at(-1);
  return {
    id: Number(id), repository: { full_name: input.repository }, head_repository: { full_name: input.repository },
    status: 'completed', conclusion: 'success', head_sha: input.sourceSHA, event: 'workflow_dispatch',
    path: id === input.buildRun ? '.github/workflows/desktop-release.yml' : '.github/workflows/desktop-check.yml',
  };
}

test('release run gate accepts only both successful runs at the exact source', async () => {
  const endpoints = [];
  const result = await verifyReleaseRuns(input, async endpoint => {
    endpoints.push(endpoint);
    return runFor(endpoint);
  });
  assert.deepEqual(result, input);
  assert.deepEqual(endpoints, [
    `repos/${input.repository}/actions/runs/${input.buildRun}`,
    `repos/${input.repository}/actions/runs/${input.sourceCheckRun}`,
  ]);
});

for (const [name, patch] of [
  ['wrong repository', { repository: 'another/repository' }],
  ['missing SHA', { sourceSHA: undefined }],
  ['short SHA', { sourceSHA: 'a'.repeat(7) }],
  ['SHA containing an endpoint', { sourceSHA: '../main' }],
  ['missing build run', { buildRun: undefined }],
  ['missing Linux run', { sourceCheckRun: undefined }],
  ['number rather than run string', { sourceCheckRun: 1234568 }],
  ['zero run', { sourceCheckRun: '0' }],
  ['signed run', { sourceCheckRun: '-1' }],
  ['noncanonical run', { sourceCheckRun: '01234568' }],
  ['unsafe run integer', { sourceCheckRun: '9007199254740992' }],
  ['injected run argument', { sourceCheckRun: '123 --repo another/repository' }],
  ['same run for both platforms', { sourceCheckRun: input.buildRun }],
]) {
  test(`release run gate rejects ${name} before contacting GitHub`, async () => {
    let calls = 0;
    await assert.rejects(verifyReleaseRuns({ ...input, ...patch }, () => { calls++; return {}; }));
    assert.equal(calls, 0);
  });
}

for (const target of ['buildRun', 'sourceCheckRun']) {
  for (const [name, patch] of [
    ['wrong run ID', { id: 2222222 }],
    ['string run ID', { id: '1234567' }],
    ['missing repository', { repository: undefined }],
    ['wrong repository', { repository: { full_name: 'another/repository' } }],
    ['fork source', { head_repository: { full_name: 'another/repository' } }],
    ['missing head repository', { head_repository: undefined }],
    ['wrong source SHA', { head_sha: 'b'.repeat(40) }],
    ['in progress', { status: 'in_progress' }],
    ['failed', { conclusion: 'failure' }],
    ['cancelled', { conclusion: 'cancelled' }],
    ['skipped', { conclusion: 'skipped' }],
    ['action required', { conclusion: 'action_required' }],
    ['wrong workflow', { path: '.github/workflows/unrelated.yml' }],
    ['nonmanual run', { event: 'pull_request' }],
  ]) {
    test(`release run gate rejects ${target}: ${name}`, async () => {
      await assert.rejects(verifyReleaseRuns(input, endpoint => {
        const run = runFor(endpoint);
        return endpoint.endsWith(`/${input[target]}`) ? { ...run, ...patch } : run;
      }), /Exact successful same-source run required/);
    });
  }

  test(`release run gate fails closed on ${target} API error without relaying sensitive output`, async () => {
    const sentinel = 'private-output-must-not-escape';
    await assert.rejects(verifyReleaseRuns(input, endpoint => {
      if (endpoint.endsWith(`/${input[target]}`)) throw new Error(sentinel);
      return runFor(endpoint);
    }), error => error.message === 'Unable to verify workflow run through GitHub API' && !error.message.includes(sentinel));
  });

  for (const response of [null, {}, '<html>unavailable</html>', []]) {
    test(`release run gate rejects malformed ${target} response ${JSON.stringify(response)}`, async () => {
      await assert.rejects(verifyReleaseRuns(input, endpoint => endpoint.endsWith(`/${input[target]}`) ? response : runFor(endpoint)),
        /Exact successful same-source run required/);
    });
  }
}

test('desktop publication requires both same-source native and Linux evidence before downloading assets', () => {
  const workflow = fs.readFileSync(path.resolve(__dirname, '../../../../.github/workflows/desktop-publish-verified.yml'), 'utf8');
  assert.match(workflow, /source_check_run_id:\s*\n\s*required: true/);
  const gate = workflow.indexOf('node remote/bridge/desktop/scripts/verify-release-runs.cjs');
  const download = workflow.indexOf('node remote/bridge/desktop/scripts/download-release-artifact.cjs');
  assert.ok(gate >= 0 && gate < download, 'both successful workflow identities must be checked before artifact download/staging');
  assert.doesNotMatch(workflow, /gh run download/);
  assert.match(workflow, /ARTIFACT_ID = \[string\]\$asset\[0\]\.id/);
  assert.match(workflow, /ARTIFACT_DIGEST = \[string\]\$asset\[0\]\.digest/);
  const extraction = workflow.indexOf('Expand-Archive -LiteralPath verified-artifact-download/artifact.zip');
  assert.ok(extraction > download && extraction < workflow.indexOf('node tested-source/remote/bridge/desktop/scripts/verify-release-artifact.cjs'));
  assert.match(workflow, /SOURCE_CHECK_RUN_ID: \$\{\{ inputs\.source_check_run_id \}\}/);
  const verify = workflow.indexOf('node tested-source/remote/bridge/desktop/scripts/verify-release-artifact.cjs');
  const reserve = workflow.indexOf('node remote/bridge/desktop/scripts/verify-release-runs.cjs reserve-tag');
  const release = workflow.indexOf('gh release create');
  assert.ok(verify >= 0 && reserve > verify && release > reserve, 'reserve an unused tag only after asset verification, then create a release using that tag');
  assert.match(workflow, /gh release create[^\r\n]+--verify-tag/);
  assert.doesNotMatch(workflow, /gh release create[^\r\n]+--target/);
});

test('release tag creation reserves only the exact tested commit without updating existing references', async () => {
  const calls = [];
  const result = await reserveReleaseTag({ ...input, version: '1.6.4' }, (endpoint, body) => {
    calls.push({ endpoint, body });
    return { ref: body.ref, object: { type: 'commit', sha: body.sha } };
  });
  assert.deepEqual(calls, [{ endpoint: `repos/${input.repository}/git/refs`, body: { ref: 'refs/tags/v1.6.4', sha: input.sourceSHA } }]);
  assert.deepEqual(result, { repository: input.repository, tag: 'v1.6.4', sourceSHA: input.sourceSHA });
});

for (const version of [undefined, 'v1.6.4', '1.6', '1.6.4\n', '../existing', '1.6.4 --force']) {
  test(`invalid version does not create a reference: ${JSON.stringify(version)}`, async () => {
    let calls = 0;
    await assert.rejects(reserveReleaseTag({ ...input, version }, () => { calls++; }));
    assert.equal(calls, 0);
  });
}

for (const failure of ['existing wrong-SHA tag', 'existing same-SHA tag', 'existing annotated tag', 'network failure after possible creation', 'forbidden']) {
  test(`release tag reservation does not overwrite or retry after ${failure}`, async () => {
    let calls = 0;
    await assert.rejects(reserveReleaseTag({ ...input, version: '1.6.4' }, () => {
      calls++;
      throw new Error(`private-output: ${failure}`);
    }), error => error.message === 'Release tag reservation failed; review existing tag or uncertain result before retrying');
    assert.equal(calls, 1);
  });
}

for (const result of [null, {}, { ref: 'refs/tags/v1.6.4', object: { type: 'commit', sha: 'b'.repeat(40) } },
  { ref: 'refs/tags/v1.6.4', object: { type: 'tag', sha: input.sourceSHA } },
  { ref: 'refs/tags/v1.6.5', object: { type: 'commit', sha: input.sourceSHA } }]) {
  test(`release tag reservation rejects unconfirmed response: ${JSON.stringify(result)}`, async () => {
    await assert.rejects(reserveReleaseTag({ ...input, version: '1.6.4' }, () => result), /not confirmed/);
  });
}
