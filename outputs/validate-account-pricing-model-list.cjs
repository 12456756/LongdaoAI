const assert = require('node:assert/strict')
const path = require('node:path')
const { chromium } = require('C:/Users/锐/.cache/codex-runtimes/codex-primary-runtime/dependencies/node/node_modules/playwright')

async function main() {
  const base = 'http://127.0.0.1:3301'
  const userAgent = 'LongdaoPricingUIValidation/1.0'
  const response = await fetch(`${base}/api/v1/auth/login`, {
    method: 'POST', headers: { 'Content-Type': 'application/json', 'User-Agent': userAgent },
    body: JSON.stringify({ email: process.env.COMPUTE_TEST_ADMIN_EMAIL, password: process.env.COMPUTE_TEST_ADMIN_PASSWORD })
  })
  assert.equal(response.status, 200)
  const { data: auth } = await response.json()
  const browser = await chromium.launch({ executablePath: 'C:/Program Files (x86)/Microsoft/Edge/Application/msedge.exe', headless: true })
  try {
    const context = await browser.newContext({ viewport: { width: 1440, height: 1060 }, userAgent })
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
    page.on('pageerror', error => errors.push(error.message))
    await page.goto(`${base}/admin/accounts`)
    await page.getByRole('button', { name: /添加账号|创建账号/ }).first().click()
    await page.getByRole('button', { name: 'OpenAI', exact: true }).click()
    await page.locator('[data-tour="account-form-type"]').getByRole('button', { name: /API Key/ }).click()
    // Inject an unsaved whitelist fixture through the same event as the selector.
    await page.locator('#create-account-form').evaluate(form => {
      const find = component => {
        if (!component) return null
        if (component.type?.__name === 'ModelWhitelistSelector') return component
        const walk = node => {
          if (!node) return null
          const found = find(node.component)
          if (found) return found
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
    const list = page.locator('.account-pricing .quote-models:visible')
    await list.waitFor()
    assert.equal(await list.locator('.quote-model').count(), 66)
    await list.scrollIntoViewIfNeeded()
    const geometry = () => list.evaluate(nav => ({
      top: nav.scrollTop, height: nav.clientHeight, total: nav.scrollHeight,
      modalTop: nav.closest('.modal-body').scrollTop,
      editorY: nav.nextElementSibling.getBoundingClientRect().top
    }))
    const before = await geometry()
    assert.ok(before.total > before.height)
    await list.hover()
    await page.mouse.wheel(0, 600)
    await page.waitForFunction(() => document.querySelector('.quote-models')?.scrollTop > 0)
    const after = await geometry()
    assert.ok(after.top > before.top)
    assert.equal(after.modalTop, before.modalTop)
    assert.equal(after.editorY, before.editorY)
    await list.evaluate(nav => { nav.scrollTop = nav.scrollHeight })
    await list.locator('.quote-model').last().click()
    await page.getByRole('heading', { name: 'upstream-model-65', exact: true }).waitFor()
    await page.screenshot({ path: path.join(__dirname, 'account-pricing-model-list-desktop.png'), animations: 'disabled' })
    await page.setViewportSize({ width: 390, height: 844 })
    await list.scrollIntoViewIfNeeded()
    const mobile = await list.evaluate(nav => ({ width: nav.clientWidth, total: nav.scrollWidth }))
    assert.ok(mobile.total > mobile.width)
    await list.evaluate(nav => { nav.scrollLeft = nav.scrollWidth })
    await list.locator('.quote-model').last().click()
    await page.screenshot({ path: path.join(__dirname, 'account-pricing-model-list-mobile.png'), animations: 'disabled' })
    assert.ok(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth + 1))
    assert.deepEqual(errors, [])
    console.log(JSON.stringify({ models: 66, desktop: { before, after }, mobile, pageErrors: errors }))
  } finally { await browser.close() }
}
main().catch(error => { console.error(error); process.exitCode = 1 })
