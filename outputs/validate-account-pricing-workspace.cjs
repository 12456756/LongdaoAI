const assert = require('node:assert/strict')
const path = require('node:path')
const { chromium } = require('C:/Users/锐/.cache/codex-runtimes/codex-primary-runtime/dependencies/node/node_modules/playwright')

async function main() {
  const base = 'http://127.0.0.1:3301'
  const response = await fetch(`${base}/api/v1/auth/login`, {
    method: 'POST', headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ email: process.env.COMPUTE_TEST_ADMIN_EMAIL, password: process.env.COMPUTE_TEST_ADMIN_PASSWORD })
  })
  assert.equal(response.status, 200)
  const { data: auth } = await response.json()
  const browser = await chromium.launch({ executablePath: 'C:/Program Files (x86)/Microsoft/Edge/Application/msedge.exe', headless: true })
  try {
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
    const errors = [], mutations = []
    page.on('pageerror', e => errors.push(e.message))
    page.on('request', r => {
      if (/\/api\/v1\/admin\/accounts(?:\/\d+)?$/.test(r.url()) && ['POST', 'PUT', 'PATCH', 'DELETE'].includes(r.method())) mutations.push(r.url())
    })
    await page.goto(`${base}/admin/accounts`)
    await page.getByRole('row').filter({ hasText: '脑生云测试' }).first().getByRole('button', { name: '编辑', exact: true }).click()
    await page.getByLabel('所属上游公司', { exact: true }).waitFor()
    await page.getByRole('tab', { name: /模型报价/ }).click()
    const editor = page.locator('.account-pricing:visible')
    const nav = editor.locator('.quote-models')
    const body = editor.locator('.quote-editor-body')
    await body.waitFor()
    assert.equal(await editor.getByLabel('所属上游公司', { exact: true }).count(), 0)
    const geometry = () => editor.evaluate(p => {
      const modal = p.closest('.modal-body'), nav = p.querySelector('.quote-models'), body = p.querySelector('.quote-editor-body')
      return { outer: modal.scrollTop, outerHeight: modal.clientHeight, outerTotal: modal.scrollHeight, list: nav.scrollTop, listHeight: nav.clientHeight, listTotal: nav.scrollHeight, form: body.scrollTop, tabs: p.closest('form').querySelector('.account-form-tabs').getBoundingClientRect().top, title: p.querySelector('.editor-title').getBoundingClientRect().top, footer: modal.nextElementSibling.getBoundingClientRect().top, page: window.scrollY }
    })
    const before = await geometry()
    assert.ok(before.listTotal > before.listHeight)
    await nav.hover()
    await page.mouse.wheel(0, 500)
    await page.waitForTimeout(200)
    const listScrolled = await geometry()
    assert.ok(listScrolled.list > 0)
    assert.equal(listScrolled.outer, 0)
    assert.equal(listScrolled.form, 0)
    assert.equal(listScrolled.tabs, before.tabs)
    await editor.locator('.advanced-pricing > summary').click()
    await body.hover()
    await page.mouse.wheel(0, 700)
    await page.waitForTimeout(200)
    const formScrolled = await geometry()
    assert.ok(formScrolled.form > 0)
    assert.equal(formScrolled.outer, 0)
    assert.equal(formScrolled.list, listScrolled.list)
    assert.equal(formScrolled.tabs, before.tabs)
    assert.equal(formScrolled.title, before.title)
    assert.equal(formScrolled.footer, before.footer)
    assert.equal(formScrolled.page, before.page)
    await body.evaluate(b => { b.scrollTop = b.scrollHeight })
    await page.mouse.wheel(0, 1200)
    await page.waitForTimeout(150)
    assert.equal((await geometry()).outer, 0)
    await editor.getByLabel('搜索报价模型').fill('gpt-5.6-luna')
    await body.evaluate(b => { b.scrollTop = 0 })
    const amount = editor.getByLabel('gpt-5.6-luna input 原价', { exact: true })
    const savedValue = await amount.inputValue()
    await amount.fill('0.222')
    await page.getByText('1 个模型报价未保存', { exact: true }).waitFor()
    await editor.getByLabel('清空模型搜索').click()
    await editor.getByRole('tab', { name: '未保存', exact: true }).click()
    assert.equal(await nav.locator('.quote-model').count(), 1)
    await editor.getByRole('tab', { name: '待填写', exact: true }).click()
    assert.ok(await nav.locator('.quote-model').count() > 0)
    await editor.getByRole('tab', { name: '全部', exact: true }).click()
    await editor.getByLabel('搜索报价模型').fill('gpt-5.6-luna')
    assert.equal(await amount.inputValue(), '0.222')
    await editor.locator('.advanced-pricing').evaluate(d => { d.open = false })
    await editor.getByLabel('清空模型搜索').click()
    await page.screenshot({ path: path.join(__dirname, 'account-pricing-workspace-desktop.png'), animations: 'disabled' })
    for (const viewport of [{ width: 1280, height: 720 }, { width: 390, height: 844 }, { width: 360, height: 740 }]) {
      await page.setViewportSize(viewport)
      await page.waitForTimeout(100)
      const g = await geometry()
      assert.ok(g.outerTotal <= g.outerHeight + 1, JSON.stringify({ viewport, g }))
      assert.ok(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth + 1))
      const rect = await page.locator('.account-editor-dialog').boundingBox()
      assert.ok(rect.y >= 0 && rect.y + rect.height <= viewport.height + 1)
      if (viewport.width < 768) {
        const picker = editor.getByLabel('选择报价模型', { exact: true })
        await picker.selectOption({ label: 'gpt-5.6-sol · 已配置' })
        await page.getByRole('heading', { name: 'gpt-5.6-sol', exact: true }).waitFor()
        await picker.selectOption({ label: 'gpt-5.6-luna · 未保存' })
        assert.equal(await amount.inputValue(), '0.222')
        await editor.getByLabel('搜索报价模型').fill('no-matching-model')
        await editor.locator('.mobile-no-results').waitFor()
        assert.equal(await editor.getByLabel('选择报价模型', { exact: true }).count(), 0)
        await editor.getByLabel('清空模型搜索').click()
        await body.hover()
        await page.mouse.wheel(0, 500)
        await page.waitForTimeout(150)
        assert.equal((await geometry()).outer, 0)
        await body.evaluate(b => { b.scrollTop = 0 })
        await page.screenshot({ path: path.join(__dirname, `account-pricing-workspace-mobile-${viewport.width}.png`), animations: 'disabled' })
      }
    }
    await page.setViewportSize({ width: 1440, height: 900 })
    await amount.fill(savedValue)
    assert.equal(await page.getByText('1 个模型报价未保存', { exact: true }).count(), 0)
    await page.getByRole('tab', { name: '账号配置', exact: true }).click()
    await page.getByLabel('所属上游公司', { exact: true }).waitFor()
    await page.getByRole('button', { name: 'Close modal', exact: true }).click()
    await page.getByRole('button', { name: /添加账号|创建账号/ }).first().click()
    await page.getByRole('button', { name: 'OpenAI', exact: true }).click()
    await page.locator('[data-tour="account-form-type"]').getByRole('button', { name: /API Key/ }).click()
    await page.locator('#create-account-form').evaluate(form => {
      const find = component => {
        if (!component) return null
        if (component.type?.__name === 'ModelWhitelistSelector') return component
        const walk = node => {
          if (!node) return null
          const match = find(node.component)
          if (match) return match
          for (const child of Array.isArray(node.children) ? node.children : []) {
            const match = walk(child)
            if (match) return match
          }
          return null
        }
        return walk(component.subTree)
      }
      let parent = form.__vueParentComponent
      while (parent && parent.type?.__name !== 'CreateAccountModal') parent = parent.parent
      const selector = find(parent)
      if (!selector) throw new Error('Whitelist selector not found')
      selector.emit('update:modelValue', Array.from({ length: 66 }, (_, i) => `upstream-model-${i}`))
    })
    await page.getByRole('tab', { name: /模型报价/ }).click()
    assert.equal(await nav.locator('.quote-model').count(), 66)
    await editor.getByLabel('搜索报价模型').fill('upstream-model-65')
    await editor.getByLabel('upstream-model-65 input 原价', { exact: true }).fill('0')
    await page.getByText('1 个模型报价未保存', { exact: true }).waitFor()
    await page.screenshot({ path: path.join(__dirname, 'account-pricing-workspace-create.png'), animations: 'disabled' })
    assert.deepEqual(errors, [])
    assert.deepEqual(mutations, [])
    console.log(JSON.stringify({ independentScroll: { before, listScrolled, formScrolled }, viewports: 4, createWhitelistModels: 66, errors, accountMutations: mutations }))
  } finally { await browser.close() }
}
main().catch(e => { console.error(e); process.exitCode = 1 })
