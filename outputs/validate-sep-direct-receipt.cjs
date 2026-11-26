const assert = require('node:assert/strict')
const fs = require('node:fs')
const path = require('node:path')
const { createRequire } = require('node:module')
const { execFileSync } = require('node:child_process')

const sepRoot = process.env.SEP_TEST_ROOT || 'D:/github/SEP-worktree-two'
const sepRequire = createRequire(path.join(sepRoot, 'backend/package.json'))
sepRequire('ts-node').register({ project: path.join(sepRoot, 'backend/tsconfig.json'), transpileOnly: true })
sepRequire('reflect-metadata')
const { ComputeProtocolClient } = sepRequire('./src/modules/compute-integration/compute-protocol.client.ts')
const { BillingResponse } = sepRequire('./src/modules/compute-integration/compute-wire-contract.ts')

async function main() {
  const execution = process.env.SEP_TEST_EXECUTION || 'r23-direct-1795404629075'
  assert.match(execution, /^[A-Za-z0-9][A-Za-z0-9_.:-]{0,127}$/)
  const keys = path.join(sepRoot, 'outputs/compute-integration/local-dev-keys-20261120')
  const publicConfig = JSON.parse(fs.readFileSync(path.join(keys, 'public-configuration.json'), 'utf8'))
  const config = {
    NODE_ENV: 'test', COMPUTE_INTEGRATION_ENABLED: 'true', COMPUTE_ALLOW_LOOPBACK_HTTP: 'true',
    COMPUTE_API_ORIGIN: 'http://127.0.0.1:28080', COMPUTE_SIGNING_KEY_ID: publicConfig.signing_key_id,
    COMPUTE_SIGNING_PRIVATE_KEY_FILE: path.join(keys, 'signing-private.pem'),
  }
  const client = new ComputeProtocolClient({ get: name => config[name] })
  const snapshot = () => JSON.parse(execFileSync('docker', ['exec', 'longdao-model-pricing-test-20261120', 'psql', '-U', 'pricing_test', '-d', 'pricing_test', '-Atc',
    `SELECT jsonb_build_object('balance',e.available_cny::text,'sequence',e.sequence::text,'ledger_count',(SELECT count(*) FROM sep_ledger WHERE enterprise_pk=e.id),'event_count',(SELECT count(*) FROM sep_balance_events WHERE enterprise_pk=e.id)) FROM sep_enterprises e JOIN sep_executions x ON x.enterprise_pk=e.id WHERE x.execution_id='${execution}'`], { encoding: 'utf8' }).trim())
  const before = snapshot()
  let committedAt
  for (let attempt = 0; attempt < 2; attempt++) {
    const bill = await client.json(`/api/v1/sep/executions/${execution}/billing`)
    const parsed = BillingResponse.safeParse(bill)
    assert.ok(parsed.success, JSON.stringify(parsed.success ? [] : parsed.error.issues))
    assert.equal(parsed.data.settlement_status, 'committed')
    const timestamp = parsed.data.settlement_receipt.committed_at
    assert.match(timestamp, /Z$/)
    if (committedAt) assert.equal(timestamp, committedAt)
    committedAt = timestamp
  }
  assert.deepEqual(snapshot(), before)
  console.log(JSON.stringify({ passed: true, execution, committed_at: committedAt, sepBillingContract: true, stableOnRepeatedQuery: true, unchangedAccounting: true, generationRequests: 0 }))
}

main().catch(error => { console.error(error.message); process.exitCode = 1 })
