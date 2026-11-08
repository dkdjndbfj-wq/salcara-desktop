'use strict';
// Run validation is read-only. The explicit reserve-tag command creates a new
// reference after verification; it never updates a tag or creates a release.
const { execFileSync } = require('node:child_process');

const RELEASE_REPOSITORY = 'dkdjndbfj-wq/salcara-desktop';
const RUNS = [
  ['buildRun', '.github/workflows/desktop-release.yml'],
  ['sourceCheckRun', '.github/workflows/desktop-check.yml'],
];

function validateSource(input) {
  if (input.repository !== RELEASE_REPOSITORY || typeof input.sourceSHA !== 'string' || !/^[a-f0-9]{40}$/.test(input.sourceSHA)) {
    throw new Error('Unexpected publishing identity');
  }
}

function validateInputs(input) {
  validateSource(input);
  for (const [key] of RUNS) {
    const value = input[key];
    if (typeof value !== 'string' || !/^[1-9][0-9]{0,15}$/.test(value) || !Number.isSafeInteger(Number(value))) {
      throw new Error('Both exact workflow run IDs are required');
    }
  }
  if (input.buildRun === input.sourceCheckRun) throw new Error('Two distinct workflow runs are required');
}

function validateRun(run, id, workflowPath, input) {
  if (!run || !Number.isSafeInteger(run.id) || String(run.id) !== id ||
      run.repository?.full_name !== input.repository || run.head_repository?.full_name !== input.repository ||
      run.status !== 'completed' || run.conclusion !== 'success' || run.head_sha !== input.sourceSHA ||
      run.path !== workflowPath || run.event !== 'workflow_dispatch') {
    throw new Error(`Exact successful same-source run required: ${workflowPath}`);
  }
}

function readRun(endpoint) {
  try {
    return JSON.parse(execFileSync('gh', ['api', '--hostname', 'github.com', endpoint], {
      encoding: 'utf8', timeout: 30000, maxBuffer: 1024 * 1024, stdio: ['ignore', 'pipe', 'pipe'],
    }));
  } catch {
    // Do not relay subprocess output, which can contain credentials or headers.
    throw new Error('Unable to verify workflow run through GitHub API');
  }
}

async function verifyReleaseRuns(input, api = readRun) {
  validateInputs(input);
  for (const [key, workflowPath] of RUNS) {
    let run;
    try {
      run = await api(`repos/${input.repository}/actions/runs/${input[key]}`);
    } catch {
      throw new Error('Unable to verify workflow run through GitHub API');
    }
    validateRun(run, input[key], workflowPath, input);
  }
  return { repository: input.repository, sourceSHA: input.sourceSHA, buildRun: input.buildRun, sourceCheckRun: input.sourceCheckRun };
}

function createReference(endpoint, reference) {
  try {
    return JSON.parse(execFileSync('gh', ['api', '--hostname', 'github.com', '--method', 'POST', endpoint,
      '-f', `ref=${reference.ref}`, '-f', `sha=${reference.sha}`], {
      encoding: 'utf8', timeout: 30000, maxBuffer: 1024 * 1024, stdio: ['ignore', 'pipe', 'pipe'],
    }));
  } catch {
    throw new Error('Release tag reservation failed; review existing tag or uncertain result before retrying');
  }
}

async function reserveReleaseTag(input, api = createReference) {
  validateSource(input);
  if (typeof input.version !== 'string' || !/^[0-9]+\.[0-9]+\.[0-9]+$/.test(input.version)) {
    throw new Error('Exact numeric release version required');
  }
  const reference = { ref: `refs/tags/v${input.version}`, sha: input.sourceSHA };
  let result;
  try {
    // Creating a new ref is atomic: an existing ref is never updated. Do not
    // preflight GET then rely on `release create --target`, which reuses tags.
    result = await api(`repos/${input.repository}/git/refs`, reference);
  } catch {
    throw new Error('Release tag reservation failed; review existing tag or uncertain result before retrying');
  }
  if (!result || result.ref !== reference.ref || result.object?.type !== 'commit' || result.object?.sha !== input.sourceSHA) {
    throw new Error('Release tag reservation was not confirmed at the tested commit');
  }
  return { repository: input.repository, tag: `v${input.version}`, sourceSHA: input.sourceSHA };
}

if (require.main === module) {
  const input = {
    repository: process.env.GITHUB_REPOSITORY,
    sourceSHA: process.env.SOURCE_SHA,
    buildRun: process.env.BUILD_RUN,
    sourceCheckRun: process.env.SOURCE_CHECK_RUN_ID,
    version: process.env.RELEASE_VERSION,
  };
  const operation = process.argv[2] === 'reserve-tag' ? reserveReleaseTag(input) :
    process.argv.length === 2 ? verifyReleaseRuns(input) : Promise.reject(new Error('Unknown release verification command'));
  operation.then(result => console.log(JSON.stringify(result))).catch(error => {
    console.error(error.message);
    process.exitCode = 1;
  });
}

module.exports = { verifyReleaseRuns, reserveReleaseTag };
