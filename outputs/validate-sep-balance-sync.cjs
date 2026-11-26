const assert = require('node:assert/strict')
const path = require('node:path')
const crypto = require('node:crypto')
const { execFileSync } = require('node:child_process')
const { chromium } = require('C:/Users/锐/.cache/codex-runtimes/codex-primary-runtime/dependencies/node/node_modules/playwright')

async function main() {
  const base = 'http://127.0.0.1:28080/api/v1', ui = 'http://127.0.0.1:3301'
  const prefix = `sep-sync-check-${Date.now()}`
  let token = '', enterpriseUser, ordinaryUser, browser
  const sql = query => execFileSync('docker', ['exec', 'longdao-model-pricing-test-20261120', 'psql', '-v', 'ON_ERROR_STOP=1', '-U', 'pricing_test', '-d', 'pricing_test', '-Atc', query], { encoding: 'utf8' }).trim()
  async function request(method, resource, body, expected = 200, key = crypto.randomUUID()) {
    const response = await fetch(base + resource, { method, headers: { 'Content-Type': 'application/json', ...(token ? { Authorization: `Bearer ${token}` } : {}), 'Idempotency-Key': key }, body: body === undefined ? undefined : JSON.stringify(body) })
    const data = await response.json()
    assert.equal(response.status, expected, `${method} ${resource}: ${JSON.stringify(data)}`)
    return data.data === undefined ? data : data.data
  }
  const auth = await request('POST', '/auth/login', { email: process.env.PRICING_TEST_EMAIL, password: process.env.PRICING_TEST_PASSWORD })
  token = auth.access_token
  const before = (await request('GET', '/admin/model-pricing')).bases
  try {
    const createUser = label => request('POST', '/admin/users', { email: `${prefix}-${label}@local.test`, password: crypto.randomUUID(), username: `${prefix}-${label}`, role: 'user', balance: 0, concurrency: 1, allowed_groups: [] })
    enterpriseUser = await createUser('enterprise')
    ordinaryUser = await createUser('ordinary')
    assert.ok(Number.isSafeInteger(enterpriseUser.id) && Number.isSafeInteger(ordinaryUser.id))
    const config = await request('GET', '/admin/sep/config')
    assert.match(config.integration_id, /^[A-Za-z0-9_.:-]+$/)
    sql(`INSERT INTO sep_enterprises(integration_id,enterprise_id,registration_ref,registration_hash,user_id,company_name,admin_email) VALUES('${config.integration_id}','${prefix}','${prefix}','${'a'.repeat(64)}',${enterpriseUser.id},'同步测试企业','${prefix}-enterprise@local.test')`)
    const operation = crypto.randomUUID()
    const body = { balance: 10, operation: 'add', notes: 'Local balance synchronization verification' }
    const posted = await request('POST', `/admin/users/${enterpriseUser.id}/balance`, body, 200, operation)
    assert.equal(posted.sep_balance.available_paid_cny, '10.000000')
    assert.equal(posted.sep_balance_receipt.committed, true)
    assert.equal(posted.sep_balance_receipt.sync_status, 'pending')
    const repeated = await request('POST', `/admin/users/${enterpriseUser.id}/balance`, body, 200, operation)
    assert.equal(repeated.sep_balance_receipt.event_id, posted.sep_balance_receipt.event_id)
    await request('POST', `/admin/users/${enterpriseUser.id}/balance`, { ...body, balance: 11 }, 409, operation)
    const failed = await request('POST', `/admin/users/${enterpriseUser.id}/balance`, { balance: 11, operation: 'subtract', notes: 'Insufficient balance check' }, 409)
    assert.equal(failed.reason, 'INSUFFICIENT_PAID_FUNDS')
    assert.ok(failed.message.includes('10.000000') && failed.message.includes('11.000000'))
    assert.equal(failed.metadata.accounting_status, 'not_committed')
    assert.ok(failed.metadata.request_id)
    await request('POST', `/admin/users/${enterpriseUser.id}/balance`, { balance: 1, operation: 'subtract', notes: 'Local adjustment' })
    await request('POST', `/admin/users/${enterpriseUser.id}/balance`, { balance: 12, operation: 'set', notes: 'Local set adjustment' })
    const ordinary = await request('POST', `/admin/users/${ordinaryUser.id}/balance`, { balance: 1, operation: 'add', notes: 'Ordinary USD regression' })
    assert.equal(ordinary.balance, 1)
    assert.equal(ordinary.sep_balance, undefined)
    assert.equal(sql(`SELECT balance FROM users WHERE id=${enterpriseUser.id}`), '0.00000000')
    const events = await request('GET', `/admin/sep/enterprises/${prefix}/balance-events?limit=2`)
    assert.equal(events.items.length, 2)
    assert.deepEqual(events.items.map(e => e.sequence), ['1', '2'])
    assert.equal(events.has_more, true)
    const last = await request('GET', `/admin/sep/enterprises/${prefix}/balance-events?after_sequence=2`)
    assert.equal(last.items.length, 1)
    assert.equal(last.items[0].payload.available_paid_cny, '12.000000')
    assert.equal(events.items[0].payload.origin, 'compute')
    if (!config.balance_webhook_url) {
      const retry = await request('POST', `/admin/sep/balance-events/${posted.sep_balance_receipt.event_id}/retry`, undefined, 409)
      assert.equal(retry.reason, 'BALANCE_WEBHOOK_NOT_CONFIGURED')
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
    const page = await context.newPage(), errors = []
    page.on('pageerror', error => errors.push(error.message))
    await page.goto(ui + '/admin/sep')
    await page.getByLabel('SEP 余额通知地址').waitFor()
    await page.getByRole('button', { name: '查看通知签名公钥' }).click()
    await page.getByText(/SEP_EVENT_SIGNING_PRIVATE_KEY/).waitFor()
    await page.screenshot({ path: path.join(__dirname, 'sep-balance-sync-config-desktop.png'), fullPage: true })
    await page.goto(ui + `/admin/sep?enterprise=${prefix}`)
    await page.getByRole('dialog').getByText('同步测试企业', { exact: true }).waitFor()
    await page.getByRole('dialog').getByText('尚未配置 SEP 接收地址，事件已保存，可补拉').first().waitFor()
    await page.getByRole('heading', { name: 'SEP 余额同步', exact: true }).scrollIntoViewIfNeeded()
    await page.waitForTimeout(250)
    await page.screenshot({ path: path.join(__dirname, 'sep-balance-sync-events-desktop.png'), animations: 'disabled' })
    await page.getByRole('dialog').getByRole('button', { name: '重试', exact: true }).first().click()
    await page.getByRole('dialog').getByRole('alert').filter({ hasText: '尚未配置 SEP 余额通知接收地址' }).waitFor()
    for (const viewport of [{ width: 390, height: 844 }, { width: 360, height: 740 }]) {
      await page.setViewportSize(viewport)
      await page.waitForTimeout(150)
      assert.ok(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth + 1))
      await page.getByRole('heading', { name: 'SEP 余额同步', exact: true }).scrollIntoViewIfNeeded()
      await page.waitForTimeout(250)
      await page.screenshot({ path: path.join(__dirname, `sep-balance-sync-events-mobile-${viewport.width}.png`), animations: 'disabled' })
    }
    await page.setViewportSize({ width: 1440, height: 900 })
    await page.goto(ui + '/admin/users')
    await page.locator('input[type="search"],input[placeholder*="搜索"]').first().fill(`${prefix}-enterprise`)
    const row = page.getByRole('row').filter({ hasText: `${prefix}-enterprise@local.test` })
    await row.getByRole('button', { name: '充值', exact: true }).click()
    await page.getByRole('dialog').getByText('¥12.00', { exact: false }).waitFor()
    await page.getByRole('dialog').getByRole('spinbutton').fill('1')
    await page.waitForTimeout(250)
    await page.screenshot({ path: path.join(__dirname, 'sep-balance-sync-recharge-desktop.png'), animations: 'disabled' })
    assert.deepEqual(errors, [])
    console.log(JSON.stringify({ passed: true, enterpriseCurrency: 'CNY', ordinaryCurrency: 'USD', idempotentRecharge: true, insufficientBalanceReason: failed.reason, atomicEvents: 3, generationRequests: 0, browserErrors: errors }))
  } finally {
    if (browser) await browser.close()
    if (enterpriseUser) {
      sql(`BEGIN; DELETE FROM sep_balance_events WHERE enterprise_pk=(SELECT id FROM sep_enterprises WHERE enterprise_id='${prefix}'); DELETE FROM sep_ledger WHERE enterprise_pk=(SELECT id FROM sep_enterprises WHERE enterprise_id='${prefix}'); DELETE FROM sep_balance_operations WHERE user_id=${enterpriseUser.id}; DELETE FROM sep_enterprises WHERE user_id=${enterpriseUser.id} AND enterprise_id='${prefix}'; COMMIT;`)
      await request('DELETE', `/admin/users/${enterpriseUser.id}`)
    }
    if (ordinaryUser) await request('DELETE', `/admin/users/${ordinaryUser.id}`)
    assert.deepEqual((await request('GET', '/admin/model-pricing')).bases, before)
  }
}
main().catch(error => { console.error(error); process.exitCode = 1 })
