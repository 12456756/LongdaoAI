const assert = require('node:assert/strict')
const path = require('node:path')
const { execFileSync } = require('node:child_process')
const { chromium } = require('C:/Users/锐/.cache/codex-runtimes/codex-primary-runtime/dependencies/node/node_modules/playwright')

async function main() {
  const base = 'http://127.0.0.1:3301'
  const userAgent = 'LongdaoExchangeValidation/1.0'
  let token = ''
  async function api(method, resource, body, status = 200) {
    const response = await fetch(`${base}/api/v1${resource}`, {
      method, headers: { 'Content-Type': 'application/json', 'User-Agent': userAgent, ...(token ? { Authorization: `Bearer ${token}` } : {}) },
      body: body === undefined ? undefined : JSON.stringify(body),
    })
    const json = await response.json()
    assert.equal(response.status, status, `${method} ${resource}: ${JSON.stringify(json)}`)
    return json.data
  }
  const auth = await api('POST', '/auth/login', { email: process.env.COMPUTE_TEST_ADMIN_EMAIL, password: process.env.COMPUTE_TEST_ADMIN_PASSWORD })
  token = auth.access_token
  await api('PUT', '/admin/settings', { platform_usd_to_cny_rate: '7.15' })
  for (const value of ['0', '-1', '', 'NaN', '7.1234567890123']) {
    await api('PUT', '/admin/settings', { platform_usd_to_cny_rate: value }, 400)
  }
  for (const resource of ['/admin/settings', '/settings/public', '/payment/config', '/payment/checkout-info', '/admin/payment/config']) {
    const config = await api('GET', resource)
    assert.equal(config.platform_usd_to_cny_rate, '7.15', resource)
    for (const old of ['balance_display_cny_rate', 'balance_recharge_multiplier', 'subscription_usd_to_cny_rate', 'payment_balance_recharge_multiplier', 'payment_subscription_usd_to_cny_rate']) {
      assert.equal(Object.hasOwn(config, old), false, `${resource} still exposes ${old}`)
    }
  }
  console.log('Admin, public settings, payment config and checkout share one rate; invalid updates rejected')

  const originalConfig = await api('GET', '/admin/payment/config')
  let fixture
  let order
  let group
  let plan
  let subscription
  try {
    await api('PUT', '/admin/payment/config', { ...originalConfig, enabled: true, balance_disabled: false, min_amount: 1, max_amount: 0, daily_limit: 0, recharge_fee_rate: 2.5, enabled_payment_types: ['alipay'], max_pending_orders: 10 })
    fixture = await api('POST', '/admin/payment/providers', {
      provider_key: 'easypay', name: 'Local exchange rate validation', supported_types: ['alipay'], enabled: true,
      payment_mode: 'popup', sort_order: 0, config: { pid: 'local-test', pkey: 'local-test-key', apiBase: 'http://127.0.0.1:28080/local-payment-test', notifyUrl: 'http://127.0.0.1:28080/api/v1/payment/webhook/easypay', returnUrl: 'http://127.0.0.1:3301/payment', paymentMode: 'popup' },
    }, 201)
    order = await api('POST', '/payment/orders', { amount: 10, payment_type: 'alipay', order_type: 'balance' })
    assert.equal(order.amount, 1.3986014)
    assert.equal(order.pay_amount, 10.25)
    const stored = await api('GET', `/admin/payment/orders/${order.order_id}`)
    const detail = stored.order || stored
    assert.equal(detail.amount, 1.3986014)
    const snapshotRate = execFileSync('docker', ['exec', 'longdao-model-pricing-test-20261120', 'psql', '-U', 'pricing_test', '-d', 'pricing_test', '-tAc', `SELECT provider_snapshot->>'platform_usd_to_cny_rate' FROM payment_orders WHERE id = ${Number(order.order_id)}`], { encoding: 'utf8' }).trim()
    assert.equal(snapshotRate, '7.15')
    await api('PUT', '/admin/settings', { platform_usd_to_cny_rate: '8' })
    const unchanged = await api('GET', `/payment/orders/${order.order_id}`)
    assert.equal(unchanged.amount, 1.3986014)
    assert.equal(unchanged.pay_amount, 10.25)
    console.log('Real CNY order persisted eight decimal balance precision, fee and rate snapshot; rate updates leave order amounts intact')
    await api('PUT', '/admin/settings', { platform_usd_to_cny_rate: '7.15' })
    group = await api('POST', '/admin/groups', { name: `exchange-test-${Date.now()}`, platform: 'openai', subscription_type: 'subscription', rate_multiplier: 1 })
    plan = await api('POST', '/admin/payment/plans', { name: 'Local exchange test', description: 'Isolated exchange validation', group_id: group.id, price: 9.99, currency: 'USD', validity_days: 30, validity_unit: 'days', for_sale: true }, 201)
    await api('POST', '/admin/payment/plans', { name: 'Rejected currency', group_id: group.id, price: 9.99, currency: 'CNY', validity_days: 30, validity_unit: 'days' }, 400)
    subscription = await api('POST', '/payment/orders', { payment_type: 'alipay', order_type: 'subscription', plan_id: plan.id })
    assert.equal(subscription.amount, 9.99)
    assert.equal(subscription.pay_amount, 73.22)
    console.log('Real subscription order converts USD 9.99 to CNY 71.43, then adds CNY 1.79 fee; non-USD plan prices rejected')
  } finally {
    if (subscription) await api('POST', `/admin/payment/orders/${subscription.order_id}/cancel`, {})
    if (order) await api('POST', `/admin/payment/orders/${order.order_id}/cancel`, {})
    if (plan) await api('DELETE', `/admin/payment/plans/${plan.id}`)
    if (group) await api('DELETE', `/admin/groups/${group.id}`)
    if (fixture) await api('DELETE', `/admin/payment/providers/${fixture.id}`)
    await api('PUT', '/admin/payment/config', originalConfig)
    await api('PUT', '/admin/settings', { platform_usd_to_cny_rate: '7.15' })
  }

  const browser = await chromium.launch({ executablePath: 'C:/Program Files (x86)/Microsoft/Edge/Application/msedge.exe', headless: true })
  try {
    const context = await browser.newContext({ viewport: { width: 1440, height: 1000 }, userAgent })
    await context.addInitScript(({ auth }) => {
      if (window !== window.top) return
      localStorage.setItem('auth_token', auth.access_token)
      localStorage.setItem('auth_user', JSON.stringify(auth.user))
      localStorage.setItem('token_expires_at', String(Date.now() + 3600000))
      localStorage.setItem('locale', 'zh-CN')
      localStorage.setItem(`onboarding_tour_${auth.user.id}_${auth.user.role}_v4_interactive`, 'true')
      localStorage.setItem(`admin_guide_${auth.user.id}_${auth.user.role}_v4_interactive`, 'true')
    }, { auth })
    const page = await context.newPage()
    const errors = []
    page.on('pageerror', error => errors.push(error.message))
    await page.goto(`${base}/admin/settings`)
    await page.getByRole('tab', { name: '支付设置', exact: true }).click()
    const rate = page.getByLabel('平台统一汇率', { exact: true })
    await rate.waitFor()
    assert.equal(await rate.inputValue(), '7.15')
    assert.equal(await page.getByText('余额充值倍率', { exact: true }).count(), 0)
    assert.equal(await page.getByText('订阅 CNY 换算汇率', { exact: true }).count(), 0)
    await rate.fill('7.123456789012')
    const saved = page.waitForResponse(response => response.url().endsWith('/admin/settings') && response.request().method() === 'PUT')
    await page.getByRole('button', { name: /保存设置/ }).click()
    assert.equal((await saved).status(), 200)
    assert.equal((await api('GET', '/settings/public')).platform_usd_to_cny_rate, '7.123456789012')
    await api('PUT', '/admin/settings', { platform_usd_to_cny_rate: '7.15' })
    await page.reload()
    await page.getByRole('tab', { name: '支付设置', exact: true }).click()
    await rate.waitFor()
    await page.screenshot({ path: path.join(__dirname, 'exchange-rate-desktop.png'), fullPage: false, animations: 'disabled' })
    await page.setViewportSize({ width: 390, height: 844 })
    await rate.scrollIntoViewIfNeeded()
    await page.screenshot({ path: path.join(__dirname, 'exchange-rate-mobile.png'), animations: 'disabled' })
    assert.ok(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth + 1), 'Mobile overflow')
    assert.deepEqual(errors, [])
    console.log('Desktop and mobile settings rendered; decimal string saved without loss of precision')
  } finally {
    await browser.close()
  }
}
main().catch(error => { console.error(error); process.exitCode = 1 })
