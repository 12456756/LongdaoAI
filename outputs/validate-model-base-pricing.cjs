const assert = require('node:assert/strict')
const path = require('node:path')
const { chromium } = require('C:/Users/锐/.cache/codex-runtimes/codex-primary-runtime/dependencies/node/node_modules/playwright')

async function main() {
  const endpoint = process.env.PRICING_TEST_URL || 'http://127.0.0.1:28080'
  const userAgent = 'LongdaoPricingValidation/1.0'
  let token = ''
  async function request(method, resource, body, expected = 200) {
    const res = await fetch(endpoint + '/api/v1' + resource, {
      method, headers: { 'Content-Type': 'application/json', 'User-Agent': userAgent, ...(token ? { Authorization: `Bearer ${token}` } : {}) },
      body: body === undefined ? undefined : JSON.stringify(body)
    })
    const json = await res.json()
    assert.equal(res.status, expected, `${method} ${resource}: ${JSON.stringify(json)}`)
    return json.data
  }
  const auth = await request('POST', '/auth/login', { email: process.env.COMPUTE_TEST_ADMIN_EMAIL, password: process.env.COMPUTE_TEST_ADMIN_PASSWORD })
  token = auth.access_token
  const proxyAuth = await fetch('http://127.0.0.1:3301/api/v1/auth/me', { headers: { Authorization: `Bearer ${token}`, 'User-Agent': userAgent, 'X-Admin-UI-Request': '1' } })
  assert.equal(proxyAuth.status, 200, `proxy auth: ${await proxyAuth.text()}`)
  const stamp = Date.now()
  const company = await request('POST', '/admin/upstream-companies', { name: `测试上游 ${stamp}`, notes: '本地隔离验证' })
  const model = `pricing-test-${stamp}`
  const price = {
    model, upstream_model: 'upstream-alias', mode: 'token', discount: '0.5', markup: '1.6', revision: 0,
    items: [
      { kind: 'input', tier: '', unit: 'USD/MTok', amount: '2' },
      { kind: 'output', tier: '', unit: 'USD/MTok', amount: '8' },
      { kind: 'cache_read', tier: '', unit: 'USD/MTok', amount: '0.2' },
      { kind: 'cache_write', tier: '', unit: 'USD/MTok', amount: '2.5' }
    ], policy: {}
  }
  const create = (name, card) => request('POST', '/admin/accounts', {
    name, platform: 'openai', type: 'apikey', credentials: { api_key: 'local-unused-key', base_url: 'https://example.invalid', model_mapping: { [model]: 'upstream-alias', [`unpriced-${stamp}`]: 'unpriced-alias' } },
    extra: {}, concurrency: 1, priority: 1, rate_multiplier: 1,
    upstream_company_id: company.id, model_prices: [card]
  })
  const accountA = await create('验证账号 A', price)
  const accountB = await create('验证账号 B', { ...price, markup: '2' })
  let list = await request('GET', `/admin/accounts/${accountA.id}/model-prices`)
  assert.equal(list.prices[0].items[0].amount, '2')
  assert.ok(list.version)
  let catalog = await request('GET', '/admin/model-pricing')
  assert.ok(catalog.accounts.find(a => a.id === accountA.id).models.includes(`unpriced-${stamp}`))
  const p = list.prices[0]
  const chosen = await request('PUT', '/admin/model-pricing/base-price', { model, source_price_id: p.id, source_revision: p.revision, expected_version: 0 })
  assert.equal(chosen.price.items[0].amount, '1.6')
  assert.equal(chosen.price.items[1].amount, '6.4')
  assert.equal(chosen.price.items[2].amount, '0.16')
  await request('PUT', '/admin/model-pricing/base-price', { model, source_price_id: p.id, source_revision: p.revision, expected_version: 0 }, 409)
  await request('PUT', `/admin/accounts/${accountA.id}`, { name: '验证账号 A 已编辑', upstream_company_id: company.id, model_prices: [{ ...p, markup: '2' }], model_prices_version: list.version })
  await request('PUT', `/admin/accounts/${accountA.id}`, { model_prices: [p], model_prices_version: list.version }, 409)
  catalog = await request('GET', '/admin/model-pricing')
  assert.equal(catalog.bases.find(b => b.model === model).price.items[0].amount, '1.6')
  const refreshed = await request('GET', `/admin/accounts/${accountA.id}/model-prices`)
  assert.equal(refreshed.prices[0].revision, 2)
  await request('PUT', `/admin/accounts/${accountA.id}`, { model_prices: refreshed.prices, model_prices_version: refreshed.version })
  const unchanged = await request('GET', `/admin/accounts/${accountA.id}/model-prices`)
  assert.equal(unchanged.prices[0].revision, 2)
  await request('DELETE', `/admin/upstream-companies/${company.id}`, undefined, 409)
  const browser = await chromium.launch({ executablePath: 'C:/Program Files (x86)/Microsoft/Edge/Application/msedge.exe', headless: true })
  try {
    const context = await browser.newContext({ viewport: { width: 1440, height: 1000 }, userAgent })
    await context.addInitScript(({ auth }) => {
      localStorage.setItem('auth_token', auth.access_token)
      localStorage.setItem('auth_user', JSON.stringify(auth.user))
      localStorage.setItem('token_expires_at', String(Date.now() + 3600000))
      localStorage.setItem('locale', 'zh-CN')
      localStorage.setItem(`onboarding_tour_${auth.user.id}_${auth.user.role}_v4_interactive`, 'true')
      localStorage.setItem(`admin_guide_${auth.user.id}_${auth.user.role}_v4_interactive`, 'true')
    }, { auth })
    const page = await context.newPage()
    const errors = []
    page.on('pageerror', e => errors.push(e.message))
    await page.goto('http://127.0.0.1:3301/admin/model-pricing')
    await page.getByRole('main').getByRole('heading', { name: '模型基础价格', exact: true }).waitFor()
    await page.getByRole('combobox', { name: '筛选上游公司' }).selectOption(String(company.id))
    await page.getByRole('searchbox', { name: '搜索模型' }).fill(model)
    await page.getByText('来源报价已变化，当前仍按已确认的基础价计费。', { exact: true }).waitFor()
    await page.screenshot({ path: path.join(__dirname, 'model-pricing-desktop.png'), fullPage: true })
    const radio = page.getByRole('radio', { name: new RegExp(`选择.*验证账号 B`) })
    await radio.check()
    await page.getByRole('button', { name: '更新基础价', exact: true }).click()
    await page.getByText('确认模型基础价格', { exact: true }).waitFor()
    await page.getByRole('button', { name: '保存基础价', exact: true }).click()
    await page.getByText('v2', { exact: true }).waitFor()
    await page.getByRole('button', { name: '价格历史', exact: true }).click()
    await page.getByRole('dialog').getByText('v1', { exact: true }).waitFor()
    await page.keyboard.press('Escape')
    await page.getByRole('dialog').waitFor({ state: 'hidden' })
    await page.setViewportSize({ width: 390, height: 844 })
    await page.waitForFunction(() => document.querySelector('main').getBoundingClientRect().left < 30 && document.querySelector('aside').getBoundingClientRect().right < 1)
    await page.screenshot({ path: path.join(__dirname, 'model-pricing-mobile.png'), fullPage: true, animations: 'disabled' })
    const size = await page.evaluate(() => ({ viewport: innerWidth, document: document.documentElement.scrollWidth }))
    assert.ok(size.document <= size.viewport + 1, JSON.stringify(size))
    assert.deepEqual(errors, [])
  } finally { await browser.close() }
  const history = await request('GET', `/admin/model-pricing/history?model=${model}`)
  assert.equal(history.length, 2)
  assert.equal(history[0].price.items[0].amount, '2')
  assert.equal(history[1].price.items[0].amount, '1.6')
  const paged = await request('GET', `/admin/model-pricing?page=1&page_size=1&search=${model}`)
  assert.deepEqual(paged.models, [model])
  assert.equal(paged.total, 1)
  const missing = await request('GET', `/admin/model-pricing?page=1&page_size=20&company_id=${company.id}&status=missing`)
  assert.ok(missing.models.includes(`unpriced-${stamp}`))
  const http = require('node:http')
  let upstreamCalls = 0
  let changeDuringRequest
  const upstream = http.createServer(async (req, res) => {
    let body = ''
    for await (const chunk of req) body += chunk
    upstreamCalls++
    try {
      assert.equal(req.url, '/v1/chat/completions')
      assert.equal(JSON.parse(body).model, 'upstream-alias')
      if (changeDuringRequest) { await changeDuringRequest(); changeDuringRequest = undefined }
      res.writeHead(200, { 'Content-Type': 'application/json' })
      res.end(JSON.stringify({ id: `chatcmpl-${stamp}-${upstreamCalls}`, object: 'chat.completion', created: 1, model: 'upstream-alias', choices: [{ index: 0, message: { role: 'assistant', content: 'local billing verification' }, finish_reason: 'stop' }], usage: { prompt_tokens: 1000, completion_tokens: 100, total_tokens: 1100, prompt_tokens_details: { cached_tokens: 400 } } }))
    } catch (e) { res.writeHead(500); res.end(String(e)) }
  })
  await new Promise(resolve => upstream.listen(0, '127.0.0.1', resolve))
  try {
    const group = await request('POST', '/admin/groups', { name: `pricing-group-${stamp}`, platform: 'openai', subscription_type: 'standard', rate_multiplier: 1.5 })
    await request('PUT', `/admin/accounts/${accountA.id}`, { group_ids: [group.id], credentials: { api_key: 'local-unused-key', base_url: `http://127.0.0.1:${upstream.address().port}`, model_mapping: { [model]: 'upstream-alias' } }, extra: { openai_responses_mode: 'force_chat_completions' } })
    await request('POST', `/admin/users/${auth.user.id}/balance`, { balance: 10, operation: 'set', notes: 'isolated local test balance' })
    const key = await request('POST', '/keys', { name: `pricing-key-${stamp}`, group_id: group.id })
    const gatewayRequest = async name => {
      const res = await fetch(endpoint + '/v1/chat/completions', { method: 'POST', headers: { Authorization: `Bearer ${key.key}`, 'Content-Type': 'application/json' }, body: JSON.stringify({ model: name, messages: [{ role: 'user', content: 'test' }] }) })
      const body = await res.json()
      return { status: res.status, body, id: res.headers.get('x-client-request-id') }
    }
    const bPrices = await request('GET', `/admin/accounts/${accountB.id}/model-prices`)
    changeDuringRequest = async () => {
      await request('PUT', `/admin/accounts/${accountB.id}`, { model_prices: [{ ...bPrices.prices[0], markup: '4' }], model_prices_version: bPrices.version })
      const changed = await request('GET', `/admin/accounts/${accountB.id}/model-prices`)
      await request('PUT', '/admin/model-pricing/base-price', { model, source_price_id: changed.prices[0].id, source_revision: changed.prices[0].revision, expected_version: 2 })
    }
    for (const expectedCost of [0.00312, 0.00624]) {
      const response = await gatewayRequest(model)
      assert.equal(response.status, 200, JSON.stringify(response.body))
      let usage
      for (let attempt = 0; attempt < 50; attempt++) {
        assert.match(response.id, /^[a-f0-9-]+$/)
        const query = `SELECT COALESCE((SELECT actual_cost::text FROM usage_logs WHERE request_id='client:${response.id}' AND api_key_id=${key.id}),'pending')`
        usage = require('node:child_process').execFileSync('docker', ['exec', 'longdao-model-pricing-test-20261120', 'psql', '-U', 'pricing_test', '-d', 'pricing_test', '-Atc', query], { encoding: 'utf8' }).trim()
        if (usage !== 'pending') break
        await new Promise(resolve => setTimeout(resolve, 100))
      }
      assert.notEqual(usage, 'pending')
      assert.ok(Math.abs(Number(usage) - expectedCost) < 1e-10, usage)
    }
    const callsBeforeMissingPrice = upstreamCalls
    const billingUser = await request('GET', '/auth/me')
    assert.ok(Math.abs(billingUser.balance - 9.99064) < 1e-8, JSON.stringify(billingUser.balance))
    const rejected = await gatewayRequest(`unpriced-${stamp}`)
    assert.equal(rejected.status, 503)
    assert.equal(rejected.body.error.code, 'model_base_price_not_configured')
    assert.equal(upstreamCalls, callsBeforeMissingPrice)
  } finally { await new Promise(resolve => upstream.close(resolve)) }
  console.log(JSON.stringify({ verified: 'real API + desktop/mobile browser + gateway settlement + in-flight price freeze', model, company_id: company.id, account_ids: [accountA.id, accountB.id], charges_usd: ['0.00312', '0.00624'] }))
}

main().catch(e => { console.error(e); process.exitCode = 1 })
