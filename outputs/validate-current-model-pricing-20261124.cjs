const assert = require('node:assert/strict')
const fs = require('node:fs')
const path = require('node:path')
const { createRequire } = require('node:module')
const { execFileSync } = require('node:child_process')

const sql = query => execFileSync('docker', ['exec', 'longdao-model-pricing-test-20261120', 'psql', '-v', 'ON_ERROR_STOP=1', '-U', 'pricing_test', '-d', 'pricing_test', '-Atc', query], { encoding: 'utf8' }).trim()
const read = query => JSON.parse(sql(query))
const scale = 10n ** 48n
function decimal(value) {
  const [whole, fraction = ''] = String(value).split('.')
  assert.match(String(value), /^\d+(\.\d+)?$/)
  assert.ok(fraction.length <= 48)
  return BigInt(whole + fraction.padEnd(48, '0'))
}
const multiply = (a, b) => a * b / scale
function fixed(value, digits = 10) {
  const divider = 10n ** BigInt(48 - digits)
  const rounded = ((value + divider / 2n) / divider).toString().padStart(digits + 1, '0')
  return digits ? `${rounded.slice(0, -digits)}.${rounded.slice(-digits)}` : rounded
}
function close(actual, expected, label, tolerance = '0.000000000051') {
  const a = decimal(actual), difference = a >= expected ? a - expected : expected - a
  assert.ok(difference <= decimal(tolerance), `${label}: actual=${actual}, expected=${fixed(expected, 12)}`)
}

async function main() {
  const snapshotQuery = `SELECT jsonb_build_object('bases',(SELECT jsonb_agg(to_jsonb(b) ORDER BY model) FROM model_base_prices b),'ledger_count',(SELECT count(*) FROM sep_ledger),'usage_count',(SELECT count(*) FROM usage_logs),'balances',(SELECT jsonb_agg(jsonb_build_array(id,balance,frozen_balance) ORDER BY id) FROM users),'enterprise_balances',(SELECT jsonb_agg(jsonb_build_array(id,available_cny,frozen_cny,sequence) ORDER BY id) FROM sep_enterprises))`
  const before = read(snapshotQuery)
  const entries = read(`SELECT jsonb_agg(jsonb_build_object('base',to_jsonb(b),'source',p.price,'source_revision',p.revision) ORDER BY b.model) FROM model_base_prices b JOIN account_model_prices p ON p.id=b.source_price_id`)
  assert.ok(entries.length)
  const baseChecks = []
  for (const { base, source, source_revision } of entries) {
    if (source_revision !== base.source_revision) {
      baseChecks.push({ model: base.model, version: base.version, status: 'source_changed_saved_snapshot_remains_effective' })
      continue
    }
    const checks = []
    for (const item of source.items) {
      let expected = multiply(multiply(decimal(item.amount), decimal(source.discount)), decimal(source.markup))
      if (source.currency === 'CNY') expected = expected * scale / decimal(base.price.conversion.usd_to_cny_rate)
      const saved = base.price.items.find(i => i.kind === item.kind && i.tier === item.tier)
      assert.ok(saved)
      close(saved.amount, expected, `${base.model}/${item.kind}/${item.tier}`, '0.000000000000000000000000000000000001')
      checks.push({ kind: item.kind, tier: item.tier, effective_usd: saved.amount })
    }
    baseChecks.push({ model: base.model, version: base.version, mode: base.price.mode, status: 'passed', items: checks })
  }
  const usages = read(`SELECT jsonb_agg(jsonb_build_object('id',u.id,'model',u.model,'requested_model',u.requested_model,'input_tokens',u.input_tokens,'output_tokens',u.output_tokens,'cache_read_tokens',u.cache_read_tokens,'cache_creation_tokens',u.cache_creation_tokens,'cache_creation_5m_tokens',u.cache_creation_5m_tokens,'cache_creation_1h_tokens',u.cache_creation_1h_tokens,'image_input_tokens',u.image_input_tokens,'image_output_tokens',u.image_output_tokens,'image_count',u.image_count,'image_size',u.image_size,'video_count',u.video_count,'video_resolution',u.video_resolution,'video_duration_seconds',u.video_duration_seconds,'rate_multiplier',u.rate_multiplier::text,'total_cost',u.total_cost::text,'actual_cost',u.actual_cost::text,'snapshot',s.price) ORDER BY u.id) FROM usage_logs u LEFT JOIN request_model_price_snapshots s ON s.request_id=u.request_id AND s.api_key_id=u.api_key_id`)
  const billChecks = []
  for (const bill of usages || []) {
    assert.ok(bill.snapshot, `Usage ${bill.id} has no saved model base price snapshot`)
    const p = bill.snapshot.price
    const rate = (kind, tier = '') => {
      const item = p.items.find(i => i.kind === kind && i.tier === tier)
      assert.ok(item, `Usage ${bill.id}: missing ${kind}/${tier}`)
      return decimal(item.amount)
    }
    let expected = 0n
    if (p.mode === 'token') {
      assert.ok(!p.policy?.long_context_threshold, 'Extend verification for long-context historical bills')
      for (const [kind, count] of [['input', bill.input_tokens], ['output', bill.output_tokens], ['cache_read', bill.cache_read_tokens], ['image_output', bill.image_output_tokens]]) if (count) expected += rate(kind) * BigInt(count) / 1000000n
      assert.equal(bill.image_input_tokens, 0, 'Extend verification for image-input historical bills')
      if (bill.cache_creation_tokens) {
        const split = p.items.some(i => i.kind === 'cache_write' && i.tier === '5m') && p.items.some(i => i.kind === 'cache_write' && i.tier === '1h')
        if (split) {
          expected += rate('cache_write', '5m') * BigInt(bill.cache_creation_5m_tokens) / 1000000n
          expected += rate('cache_write', '1h') * BigInt(bill.cache_creation_1h_tokens) / 1000000n
        } else expected += rate('cache_write') * BigInt(bill.cache_creation_tokens) / 1000000n
      }
    } else if (p.mode === 'image') {
      const tier = bill.image_size === '2K' || bill.image_size === '4K' ? bill.image_size : '1K'
      expected = rate('image', tier) * BigInt(bill.image_count)
    } else if (p.mode === 'video') {
      expected = rate('video', bill.video_resolution) * BigInt(bill.video_count) * BigInt(bill.video_duration_seconds)
    } else throw new Error(`Unsupported historical mode ${p.mode}`)
    close(bill.total_cost, expected, `Usage ${bill.id} total`)
    const charged = multiply(expected, decimal(bill.rate_multiplier))
    close(bill.actual_cost, charged, `Usage ${bill.id} charged`)
    const active = entries.find(e => e.base.model === bill.snapshot.model)?.base
    billChecks.push({ id: bill.id, model: bill.snapshot.model, snapshot_version: bill.snapshot.version, current_version: active?.version, current_price_matches_snapshot: JSON.stringify(active?.price.items) === JSON.stringify(p.items), actual_usd: bill.actual_cost, recomputed_usd: fixed(charged), status: 'passed' })
  }
  const sepRoot = 'D:/github/SEP-worktree-two'
  const sepRequire = createRequire(path.join(sepRoot, 'backend/package.json'))
  sepRequire('ts-node').register({ project: path.join(sepRoot, 'backend/tsconfig.json'), transpileOnly: true })
  sepRequire('reflect-metadata')
  const { ComputeProtocolClient } = sepRequire('./src/modules/compute-integration/compute-protocol.client.ts')
  const keys = path.join(sepRoot, 'outputs/compute-integration/local-dev-keys-20261120')
  const publicConfig = JSON.parse(fs.readFileSync(path.join(keys, 'public-configuration.json'), 'utf8'))
  const config = { NODE_ENV: 'test', COMPUTE_INTEGRATION_ENABLED: 'true', COMPUTE_ALLOW_LOOPBACK_HTTP: 'true', COMPUTE_API_ORIGIN: 'http://127.0.0.1:28080', COMPUTE_SIGNING_KEY_ID: publicConfig.signing_key_id, COMPUTE_SIGNING_PRIVATE_KEY_FILE: path.join(keys, 'signing-private.pem') }
  const client = new ComputeProtocolClient({ get: name => config[name] })
  const quoteChecks = []
  for (const { base } of entries) {
    if (base.price.mode !== 'image' && base.price.mode !== 'video') continue
    for (const item of base.price.items) {
      const image = base.price.mode === 'image'
      const request = image ? { model: base.model, operation: 'generate', size: item.tier, quality: 'medium', n: 1 }
        : { model: base.model, operation: 'text-to-video', resolution: item.tier, duration_seconds: 4 }
      const response = await client.json(`/api/v1/sep/validation/${image ? 'image' : 'video'}/pricing`, { method: 'POST', body: request })
      assert.equal(response.pricing_status, 'quoted', JSON.stringify(response))
      assert.equal(response.pricing_source, `${base.model}:v${base.version}`)
      const expected = multiply(decimal(item.amount) * BigInt(image ? 1 : 4), decimal(response.effective_rate_multiplier))
      assert.equal(response.currency, 'CNY')
      const fx = read("SELECT to_jsonb(value) FROM settings WHERE key='platform_usd_to_cny_rate'")
      const expectedCNY = multiply(expected, decimal(fx))
      close(response.amount_cny, expectedCNY, `${base.model}/${item.tier} quote`, '0.00000051')
      quoteChecks.push({ model: base.model, tier: item.tier, count: 1, seconds: image ? undefined : 4, multiplier: response.effective_rate_multiplier, source: response.pricing_source, actual_cny: response.amount_cny, recomputed_cny: fixed(expectedCNY, 6), recomputed_usd: fixed(expected, 12), status: 'passed' })
    }
  }
  assert.deepEqual(read(snapshotQuery), before, 'Read-only verification must not change prices, balances, ledgers or usage count')
  const report = { checked_at: new Date().toISOString(), current_base_count: entries.length, historical_bill_count: billChecks.length, quote_count: quoteChecks.length, generation_requests: 0, existing_balances_and_prices_unchanged: true, bases: baseChecks, bills: billChecks, quotes: quoteChecks }
  fs.writeFileSync(path.join(__dirname, 'current-model-pricing-evidence-20261124.json'), JSON.stringify(report, null, 2))
  console.log(JSON.stringify({ passed: true, bases: entries.length, bills: billChecks.length, quotes: quoteChecks.length, generation_requests: 0, existing_balances_and_prices_unchanged: true }))
}
main().catch(error => { console.error(error.getResponse ? JSON.stringify(error.getResponse()) : error.message); process.exitCode = 1 })
