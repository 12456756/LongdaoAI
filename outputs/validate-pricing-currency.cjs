const assert = require('node:assert/strict')
const path = require('node:path')
const { execFileSync } = require('node:child_process')
const { chromium } = require('C:/Users/锐/.cache/codex-runtimes/codex-primary-runtime/dependencies/node/node_modules/playwright')

async function main() {
  let token = '', company, browser
  const accounts = [], errors = []
  const prefix = `currency-check-${Date.now()}`
  async function request(method, resource, body, expected = 200) {
    const res = await fetch('http://127.0.0.1:28080/api/v1' + resource, {
      method, headers: { 'Content-Type': 'application/json', ...(token ? { Authorization: `Bearer ${token}` } : {}) },
      body: body === undefined ? undefined : JSON.stringify(body)
    })
    const json = await res.json()
    assert.equal(res.status, expected, `${method} ${resource}: ${json.message || ''}`)
    return json.data
  }
  const auth = await request('POST', '/auth/login', { email: process.env.PRICING_TEST_EMAIL, password: process.env.PRICING_TEST_PASSWORD })
  token = auth.access_token
  const before = await request('GET', '/admin/model-pricing')
  const rate = await request('GET', '/admin/model-pricing/exchange-rate')
  assert.equal(rate.usd_to_cny_rate, '7.15')
  const models = ['token', 'image', 'video', 'per_request'].map(mode => `${prefix}-${mode}`)
  try {
    company = await request('POST', '/admin/upstream-companies', { name: prefix, notes: 'Temporary local currency verification' })
    for (const currency of ['USD', 'CNY']) {
      const unit = mode => `${currency}/${({ token: 'MTok', image: 'image', video: 'second', per_request: 'request' })[mode]}`
      const amount = currency === 'CNY' ? '1.43' : '0.2'
      const prices = ['token', 'image', 'video', 'per_request'].map((mode, i) => ({
        model: models[i], upstream_model: models[i], mode, ...(currency === 'CNY' ? { currency } : {}), discount: '0.5', markup: '2', revision: 0, policy: {},
        items: mode === 'token' ? [
          { kind: 'input', tier: '', unit: unit(mode), amount },
          { kind: 'output', tier: '', unit: unit(mode), amount: '0' },
          { kind: 'cache_read', tier: '', unit: unit(mode), amount: '0' },
          { kind: 'cache_write', tier: '5m', unit: unit(mode), amount },
          { kind: 'cache_write', tier: '1h', unit: unit(mode), amount }
        ] : [{ kind: ({ image: 'image', video: 'video', per_request: 'request' })[mode], tier: ({ image: '2K', video: '480p', per_request: '' })[mode], unit: unit(mode), amount }]
      }))
      const account = await request('POST', '/admin/accounts', {
        name: `${prefix}-${currency}`, platform: 'openai', type: 'apikey',
        credentials: { api_key: 'local-unused-key', base_url: 'https://example.invalid', model_mapping: Object.fromEntries(models.map(m => [m, m])) },
        extra: {}, concurrency: 1, priority: 1, rate_multiplier: 1, upstream_company_id: company.id, model_prices: prices
      })
      accounts.push(account.id)
      const saved = await request('GET', `/admin/accounts/${account.id}/model-prices`)
      assert.equal(saved.usd_to_cny_rate, '7.15')
      assert.equal(saved.prices.length, 4)
      for (const price of saved.prices) assert.equal(price.currency || 'USD', currency)
      // An unchanged round trip must preserve revisions, including legacy USD quotes.
      await request('PUT', `/admin/accounts/${account.id}`, { model_prices: saved.prices.map(p => ({ ...p, currency })), model_prices_version: saved.version })
      const unchanged = await request('GET', `/admin/accounts/${account.id}/model-prices`)
      assert.ok(unchanged.prices.every(p => p.revision === 1))
      for (const price of saved.prices) {
        const body = { model: price.model, source_price_id: price.id, source_revision: price.revision, expected_version: currency === 'CNY' ? 1 : 0 }
        if (currency === 'CNY') {
          await request('PUT', '/admin/model-pricing/base-price', body, 409)
          await request('PUT', '/admin/model-pricing/base-price', { ...body, expected_exchange_rate: '7' }, 409)
        }
        const selected = await request('PUT', '/admin/model-pricing/base-price', { ...body, expected_exchange_rate: '7.15' })
        assert.equal(selected.price.currency, 'USD')
        assert.equal(selected.price.conversion.source_currency, currency)
        assert.equal(selected.price.conversion.usd_to_cny_rate, '7.15')
        assert.equal(selected.price.items[0].amount, '0.2')
        assert.equal(selected.price.conversion.source_items[0].amount, amount)
        assert.ok(selected.price.items.every(i => i.unit.startsWith('USD/')))
        if (price.mode === 'token') assert.deepEqual(selected.price.items.map(i => i.amount), ['0.2', '0', '0', '0.2', '0.2'])
      }
    }
    for (const model of models) {
      const history = await request('GET', `/admin/model-pricing/history?model=${model}`)
      assert.equal(history.length, 2)
      assert.deepEqual(history.map(h => h.price.conversion.source_currency), ['CNY', 'USD'])
    }
    browser = await chromium.launch({ executablePath: 'C:/Program Files (x86)/Microsoft/Edge/Application/msedge.exe', headless: true })
    const context = await browser.newContext({ viewport: { width: 1440, height: 900 } })
    await context.addInitScript(({ auth }) => {
      localStorage.setItem('auth_token', auth.access_token)
      localStorage.setItem('auth_user', JSON.stringify(auth.user))
      localStorage.setItem('token_expires_at', String(Date.now() + 3600000))
      localStorage.setItem('locale', 'zh-CN')
      localStorage.setItem(`onboarding_tour_${auth.user.id}_${auth.user.role}_v4_interactive`, 'true')
      localStorage.setItem(`admin_guide_${auth.user.id}_${auth.user.role}_v4_interactive`, 'true')
    }, { auth })
    const page = await context.newPage()
    page.on('pageerror', e => errors.push(e.message))
    await page.goto('http://127.0.0.1:3301/admin/model-pricing')
    await page.getByLabel('搜索模型', { exact: true }).fill(models[0])
    await page.getByRole('heading', { name: models[0], exact: true }).waitFor()
    await page.locator('.account-meta').filter({ hasText: '原报价 CNY' }).waitFor()
    await page.locator('.pricing-workspace[aria-busy="false"]').waitFor()
    assert.equal(await page.locator('.comparison-table .quote-amount').count(), 10)
    await page.screenshot({ path: path.join(__dirname, 'pricing-currency-comparison-desktop.png'), fullPage: true })
    await page.setViewportSize({ width: 390, height: 844 })
    await page.waitForTimeout(200)
    assert.ok(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth + 1))
    await page.screenshot({ path: path.join(__dirname, 'pricing-currency-comparison-mobile.png'), fullPage: true })
    await page.setViewportSize({ width: 1440, height: 900 })
    await page.goto('http://127.0.0.1:3301/admin/accounts')
    const search = page.locator('input[type="search"], input[placeholder*="搜索"]').first()
    await search.fill(prefix + '-CNY')
    const row = page.getByRole('row').filter({ hasText: prefix + '-CNY' })
    await row.getByRole('button', { name: '编辑', exact: true }).click()
    await page.getByRole('tab', { name: /模型报价/ }).click()
    const editor = page.locator('.account-pricing:visible')
    await editor.getByLabel('报价币种', { exact: true }).waitFor()
    await editor.getByLabel('搜索报价模型').fill(models[0])
    assert.equal(await editor.getByLabel('报价币种', { exact: true }).inputValue(), 'CNY')
    assert.equal(await editor.getByLabel(`${models[0]} input 原价`, { exact: true }).inputValue(), '1.43')
    await page.screenshot({ path: path.join(__dirname, 'pricing-currency-editor-desktop.png') })
    for (const viewport of [{ width: 390, height: 844 }, { width: 360, height: 740 }]) {
      await page.setViewportSize(viewport)
      await page.waitForTimeout(150)
      assert.ok(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth + 1))
      const box = await page.locator('.account-editor-dialog').boundingBox()
      assert.ok(box.y >= 0 && box.y + box.height <= viewport.height + 1)
      assert.ok(await editor.locator('.quote-editor-body').evaluate(b => b.scrollHeight > b.clientHeight))
      await page.screenshot({ path: path.join(__dirname, `pricing-currency-editor-mobile-${viewport.width}.png`) })
    }
    assert.deepEqual(errors, [])
    console.log(JSON.stringify({ passed: true, billingModes: 4, currencies: ['USD', 'CNY'], snapshots: 8, zeroPrices: true, staleRateRejection: true, unchangedRevisions: true, browserErrors: errors, realGenerationRequests: 0 }))
  } finally {
    if (browser) await browser.close()
    for (const id of accounts) await request('DELETE', `/admin/accounts/${id}`).catch(() => {})
    if (accounts.length) {
      const ids = accounts.map(Number).join(',')
      const sql = `BEGIN; DELETE FROM model_base_price_history WHERE model LIKE '${prefix}-%'; DELETE FROM model_base_prices WHERE model LIKE '${prefix}-%'; DELETE FROM account_model_prices WHERE account_id IN (${ids}); UPDATE accounts SET upstream_company_id=NULL WHERE id IN (${ids}) AND deleted_at IS NOT NULL; DELETE FROM upstream_companies WHERE id=${Number(company.id)}; COMMIT;`
      execFileSync('docker', ['exec', 'longdao-model-pricing-test-20261120', 'psql', '-v', 'ON_ERROR_STOP=1', '-U', 'pricing_test', '-d', 'pricing_test', '-c', sql], { stdio: 'pipe' })
    }
    const after = await request('GET', '/admin/model-pricing')
    assert.deepEqual(after.bases, before.bases, 'Existing effective prices must remain unchanged')
  }
}
main().catch(e => { console.error(e); process.exitCode = 1 })
