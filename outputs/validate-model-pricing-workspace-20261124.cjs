const assert = require('node:assert/strict')
const path = require('node:path')
const { chromium } = require('C:/Users/锐/.cache/codex-runtimes/codex-primary-runtime/dependencies/node/node_modules/playwright')

const base = 'http://127.0.0.1:3301'
const primaryModel = 'gpt-4.1'
const models = [primaryModel, ...Array.from({ length: 39 }, (_, i) => `model-${String(i + 1).padStart(2, '0')}`)]
const items = [
  { kind: 'input', tier: '', unit: 'USD/MTok', amount: '2' },
  { kind: 'output', tier: '', unit: 'USD/MTok', amount: '8' },
  { kind: 'cache_read', tier: '', unit: 'USD/MTok', amount: '0.5' },
  { kind: 'cache_write', tier: '5m', unit: 'USD/MTok', amount: '2.5' },
  { kind: 'cache_write', tier: '1h', unit: 'USD/MTok', amount: '4' },
  { kind: 'image_input', tier: '', unit: 'USD/MTok', amount: '3' },
  { kind: 'image_output', tier: '', unit: 'USD/MTok', amount: '12' },
]
const companyNames = ['脑生云', '云端算力', '模型服务中心']
const accounts = Array.from({ length: 25 }, (_, i) => ({
  id: i + 1, name: `生产账号 ${String(i + 1).padStart(2, '0')}`, company_id: Math.floor(i / 9) + 1,
  company_name: companyNames[Math.floor(i / 9)], platform: 'openai', status: i === 9 ? 'disabled' : 'active', models,
  prices: i === 24 ? [] : models.map((model, m) => ({
    id: (i + 1) * 100 + m, account_id: i + 1, model, upstream_model: model, mode: 'token', currency: i % 2 ? 'CNY' : 'USD',
    discount: '0.8', markup: '1.25', revision: 2,
    items: items.filter((_, n) => i % 3 || n !== 4).map(item => ({ ...item, unit: i % 2 ? 'CNY/MTok' : item.unit, amount: (Number(item.amount) * (i % 2 ? 7.15 : 1) * (1 + i / 10)).toFixed(6).replace(/\.?0+$/, '') })),
    policy: i === 1 ? { long_context_threshold: 200000, long_context_input_multiplier: 2, long_context_output_multiplier: 1.5 } : {},
  })),
}))
const bases = [{ model: primaryModel, source_price_id: 100, source_revision: 2, version: 3, updated_by: 1, updated_at: '2026-11-24T03:00:00Z', price: { ...accounts[0].prices[0], items: structuredClone(items) } }]
for (const account of accounts) for (const price of account.prices) {
  if (price.model !== 'model-01' && price.model !== 'model-02') continue
  const kind = price.model === 'model-01' ? 'image' : 'video'
  price.mode = kind
  price.items = [{ kind, tier: kind === 'image' ? '1024x1024' : '480p', unit: `${price.currency}/${kind === 'image' ? 'image' : 'second'}`, amount: '0.2' }]
}

async function main() {
  const login = await fetch(base + '/api/v1/auth/login', { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ email: process.env.PRICING_TEST_EMAIL, password: process.env.PRICING_TEST_PASSWORD }) })
  assert.equal(login.status, 200)
  const { data: auth } = await login.json()
  const readActual = async () => {
    const result = await fetch(base + '/api/v1/admin/model-pricing', { headers: { Authorization: `Bearer ${auth.access_token}` } })
    assert.equal(result.status, 200)
    return (await result.json()).data.bases
  }
  const before = await readActual()
  const browser = await chromium.launch({ executablePath: 'C:/Program Files (x86)/Microsoft/Edge/Application/msedge.exe', headless: true })
  try {
    const context = await browser.newContext({ viewport: { width: 1440, height: 900 } })
    await context.addInitScript(({ auth }) => {
      localStorage.setItem('auth_token', auth.access_token)
      localStorage.setItem('auth_user', JSON.stringify(auth.user))
      localStorage.setItem('token_expires_at', String(Date.now() + 3600000))
      localStorage.setItem('locale', 'zh-CN')
      for (const key of [`onboarding_tour_${auth.user.id}_${auth.user.role}_v4_interactive`, `admin_guide_${auth.user.id}_${auth.user.role}_v4_interactive`]) localStorage.setItem(key, 'true')
    }, { auth })
    const page = await context.newPage(), errors = [], results = []
    page.on('pageerror', error => errors.push(error.message))
    await page.goto(base + '/admin/model-pricing')
    await page.waitForFunction(() => document.querySelector('.pricing-workspace')?.getAttribute('aria-busy') === 'false')
    await page.screenshot({ path: path.join(__dirname, 'model-pricing-fixed-live-desktop.png'), animations: 'disabled' })
    let saved
    await page.route('**/api/v1/admin/model-pricing**', async route => {
      const url = new URL(route.request().url())
      if (url.pathname.endsWith('/history')) return route.fulfill({ json: { code: 0, data: bases } })
      if (route.request().method() === 'PUT') {
        saved = route.request().postDataJSON()
        return route.fulfill({ json: { code: 0, data: bases[0] } })
      }
      const pageNumber = Number(url.searchParams.get('page') || 1)
      const search = url.searchParams.get('search') || ''
      const filtered = models.filter(model => model.includes(search))
      return route.fulfill({ json: { code: 0, data: { accounts, bases, models: filtered.slice((pageNumber - 1) * 20, pageNumber * 20), page: pageNumber, page_size: 20, total: filtered.length, usd_to_cny_rate: '7.15' } } })
    })
    await page.route('**/api/v1/admin/upstream-companies', route => route.fulfill({ json: { code: 0, data: companyNames.map((name, i) => ({ id: i + 1, name, notes: '' })) } }))
    await page.reload()
    await page.getByRole('heading', { name: primaryModel, exact: true }).waitFor()
    assert.equal(await page.locator('.comparison-table tbody tr').count(), 24)
    assert.equal(await page.locator('.comparison-table thead .price-column').count(), 7)
    assert.equal(await page.locator('.effective-items').count(), 0)
    async function geometry() {
      return page.evaluate(() => {
        const rect = selector => {
          const r = document.querySelector(selector).getBoundingClientRect()
          return { x: r.x, y: r.y, right: r.right, bottom: r.bottom, width: r.width, height: r.height }
        }
        const scroller = document.querySelector('.comparison-scroll')
        return { width: innerWidth, height: innerHeight, documentWidth: document.documentElement.scrollWidth, documentHeight: document.documentElement.scrollHeight,
          pageY: scrollY, footer: rect('.selection-footer'), table: rect('.comparison-scroll'), header: rect('.comparison-table thead th'), account: rect('.comparison-table tbody th'),
          scrollTop: scroller.scrollTop, scrollLeft: scroller.scrollLeft, tableScrollHeight: scroller.scrollHeight, listScrollTop: document.querySelector('.model-list-content').scrollTop }
      })
    }
    for (const viewport of [{ width: 1440, height: 900 }, { width: 1280, height: 720 }, { width: 390, height: 844 }, { width: 360, height: 740 }]) {
      await page.setViewportSize(viewport)
      await page.waitForTimeout(350)
      await page.locator('.comparison-scroll').evaluate(el => { el.scrollTop = 0; el.scrollLeft = 0 })
      await page.locator('.model-list-content').evaluate(el => { el.scrollTop = 0 })
      const start = await geometry()
      assert.ok(start.documentWidth <= viewport.width + 1, JSON.stringify(start))
      assert.ok(start.documentHeight <= viewport.height + 1, JSON.stringify(start))
      assert.ok(start.footer.bottom <= viewport.height, JSON.stringify(start))
      assert.ok(start.table.height > 130, JSON.stringify(start))
      const box = await page.locator('.comparison-scroll').boundingBox()
      await page.mouse.move(box.x + box.width - 30, box.y + Math.min(100, box.height / 2))
      await page.mouse.wheel(0, 500)
      await page.waitForTimeout(250)
      const vertical = await geometry()
      assert.ok(vertical.scrollTop > 0, JSON.stringify(vertical))
      assert.equal(vertical.pageY, 0)
      assert.equal(vertical.footer.y, start.footer.y)
      assert.ok(Math.abs(vertical.header.y - start.header.y) < 1)
      assert.equal(vertical.listScrollTop, start.listScrollTop)
      await page.locator('.comparison-scroll').evaluate(el => { el.scrollLeft = 250 })
      const horizontal = await geometry()
      assert.ok(horizontal.scrollLeft > 0)
      assert.ok(Math.abs(horizontal.account.x - start.account.x) < 1)
      if (viewport.width > 767) {
        const list = page.locator('.model-list-content'), listBox = await list.boundingBox()
        await page.mouse.move(listBox.x + 30, listBox.y + 90)
        await page.mouse.wheel(0, 300)
        await page.waitForTimeout(200)
        const changed = await geometry()
        assert.ok(changed.listScrollTop > start.listScrollTop)
        assert.equal(changed.scrollTop, horizontal.scrollTop)
        assert.equal(changed.pageY, 0)
      } else {
        await page.getByRole('button', { name: '筛选模型', exact: true }).click()
        await page.getByLabel('筛选上游公司', { exact: true }).waitFor({ state: 'visible' })
        assert.ok((await geometry()).documentHeight <= viewport.height + 1)
        await page.getByRole('button', { name: '筛选模型', exact: true }).click()
        await page.getByRole('combobox', { name: '选择模型', exact: true }).selectOption('model-01')
        await page.getByRole('heading', { name: 'model-01', exact: true }).waitFor()
        await page.getByRole('combobox', { name: '选择模型', exact: true }).selectOption(primaryModel)
      }
      await page.locator('.comparison-scroll').evaluate(el => { el.scrollTop = 0; el.scrollLeft = 0 })
      await page.locator('.model-list-content').evaluate(el => { el.scrollTop = 0 })
      await page.screenshot({ path: path.join(__dirname, `model-pricing-vertical-${viewport.width}.png`), animations: 'disabled' })
      results.push({ ...viewport, independentScroll: true, stickyHeaders: true, stickyAccount: true, fixedFooter: true })
    }
    await page.getByRole('button', { name: '查看生效价', exact: true }).click()
    await page.getByRole('dialog').getByText('生效基础价', { exact: true }).waitFor()
    await page.getByRole('dialog').getByRole('button', { name: 'Close modal' }).click()
    await page.getByRole('button', { name: '生产账号 02 报价详情', exact: true }).click()
    await page.getByRole('dialog').getByText(/公司加价 ×1.25/).waitFor()
    await page.getByRole('dialog').getByText(/超过 200000 Token/).waitFor()
    await page.getByRole('dialog').getByRole('button', { name: 'Close modal' }).click()
    await page.getByRole('radio', { name: `${primaryModel} 选择 脑生云 生产账号 02`, exact: true }).check()
    await page.locator('.selection-footer button').click()
    await page.getByRole('dialog').getByText('新基础价 · USD', { exact: true }).waitFor()
    await page.getByRole('dialog').getByRole('button', { name: '保存基础价', exact: true }).click()
    await page.getByRole('status').filter({ hasText: '基础价格已生效' }).waitFor()
    assert.equal(saved.source_price_id, 200)
    assert.equal(saved.expected_exchange_rate, '7.15')
    await page.setViewportSize({ width: 1440, height: 900 })
    await page.getByRole('button', { name: '上游公司', exact: true }).click()
    await page.getByRole('dialog').getByLabel('公司名称', { exact: true }).waitFor()
    await page.getByRole('dialog').getByRole('button', { name: 'Close modal' }).click()
    await page.getByRole('button', { name: '价格历史', exact: true }).click()
    await page.getByRole('dialog').locator('.history-entry').waitFor()
    await page.getByRole('dialog').getByRole('button', { name: 'Close modal' }).click()
    await page.getByRole('button', { name: '显示账号', exact: true }).click()
    await page.getByRole('dialog').getByText('未配置报价', { exact: true }).waitFor()
    await page.getByRole('dialog').getByRole('checkbox').first().uncheck()
    await page.getByRole('dialog').getByRole('button', { name: 'Close modal' }).click()
    assert.equal(await page.locator('.comparison-table tbody tr').count(), 23)
    await page.locator('.model-option').filter({ hasText: 'model-01' }).click()
    await page.getByRole('columnheader', { name: /图片 · 1024x1024/ }).waitFor()
    await page.locator('.model-option').filter({ hasText: 'model-02' }).click()
    await page.getByRole('columnheader', { name: /视频 · 480p/ }).waitFor()
    await page.locator('.model-option').filter({ hasText: primaryModel }).click()
    await page.evaluate(() => document.documentElement.classList.add('dark'))
    const dark = await page.locator('.comparison-table tbody td').first().evaluate(el => ({ background: getComputedStyle(el).backgroundColor, text: getComputedStyle(el).color }))
    assert.equal(dark.background, 'rgb(17, 24, 39)')
    assert.equal(dark.text, 'rgb(229, 231, 235)')
    await page.screenshot({ path: path.join(__dirname, 'model-pricing-vertical-dark.png'), animations: 'disabled' })
    await page.evaluate(() => document.documentElement.classList.remove('dark'))
    await page.getByRole('button', { name: '下一页', exact: true }).click()
    await page.getByRole('heading', { name: 'model-20', exact: true }).waitFor()
    await page.getByRole('searchbox', { name: '搜索模型', exact: true }).fill('no-matching-model')
    await page.getByText('没有匹配的模型', { exact: true }).last().waitFor()
    assert.deepEqual(errors, [])
    assert.deepEqual(await readActual(), before)
    console.log(JSON.stringify({ passed: true, viewports: results, rowComparison: true, quoteDetails: true, effectivePriceOnDemand: true, previewAndSave: true, companyAndHistory: true, accountVisibility: true, imageAndVideo: true, paginationAndEmpty: true, existingPricesUnchanged: true, browserErrors: errors }))
  } finally { await browser.close() }
}
main().catch(error => { console.error(error); process.exitCode = 1 })
