import { defineComponent } from 'vue'
import { mount, flushPromises } from '@vue/test-utils'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import ModelPricingView from '../ModelPricingView.vue'
import { changePriceCurrency, newModelPrice } from '@/api/admin/modelPricing'

const api = vi.hoisted(() => ({ catalog: vi.fn(), companies: vi.fn(), select: vi.fn() }))
vi.mock('@/api/admin/modelPricing', async (original) => ({ ...await original<typeof import('@/api/admin/modelPricing')>(), modelPricingAPI: api }))
const dialog = defineComponent({ props: ['show'], template: '<div v-if="show"><slot /><slot name="footer" /></div>' })

describe('model base pricing selection', () => {
  beforeEach(() => {
    const price = newModelPrice('custom')
    price.id = 4; price.revision = 2; price.items[0].amount = '2'; price.items[1].amount = '5'
    api.catalog.mockResolvedValue({ accounts: [{ id: 1, name: '账号 A', company_id: 1, company_name: '上游 A', platform: 'openai', status: 'active', models: ['custom', 'unpriced'], prices: [price] }], bases: [] })
    api.companies.mockResolvedValue([])
    api.select.mockReset()
  })
  const render = () => mount(ModelPricingView, { global: { stubs: { AppLayout: { template: '<main><slot /></main>' }, BaseDialog: dialog, Icon: true } } })

  it('compares yuan and dollar quotes in shared USD rows and submits the preview exchange rate', async () => {
    const fixture = await api.catalog()
    const yuan = newModelPrice('custom')
    changePriceCurrency(yuan, 'CNY')
    yuan.id = 5; yuan.revision = 1
    yuan.items[0].amount = '1.43'; yuan.items[1].amount = '7.15'
    api.catalog.mockResolvedValue({ ...fixture, usd_to_cny_rate: '7.15', accounts: [...fixture.accounts, { ...fixture.accounts[0], id: 2, name: '人民币账号', prices: [yuan] }] })
    api.select.mockResolvedValue({})
    const wrapper = render()
    await flushPromises()
    expect(wrapper.findAll('.comparison-table tbody tr').filter(row => row.find('.quote-amount').exists())).toHaveLength(2)
    const accountRows = wrapper.findAll('.comparison-table tbody tr')
    expect(accountRows[0].get('th[scope="row"]').text()).toContain('账号 A')
    expect(accountRows[1].get('th[scope="row"]').text()).toContain('人民币账号')
    expect(accountRows[1].findAll('td')[0].get('.quote-amount').text()).toBe('0.2')
    expect(wrapper.findAll('.comparison-table thead .price-column')).toHaveLength(2)
    await wrapper.findAll('input[type="radio"]')[1].setValue(true)
    await wrapper.get('.selection-footer button').trigger('click')
    expect(wrapper.text()).toContain('确认汇率 1 USD = 7.15 CNY')
    expect(wrapper.findAll('.confirmation-table .new-price').map(td => td.text())).toEqual(['0.2', '1'])
    await wrapper.findAll('button').find(b => b.text() === '保存基础价')!.trigger('click')
    await flushPromises()
    expect(api.select).toHaveBeenCalledWith('custom', expect.objectContaining({ id: 5, currency: 'CNY' }), 0, '7.15')
  })

  it('allows explicit yuan reconfirmation after an FX change without changing the stored effective price', async () => {
    const fixture = await api.catalog()
    const source = fixture.accounts[0].prices[0]
    changePriceCurrency(source, 'CNY')
    api.catalog.mockResolvedValue({ ...fixture, usd_to_cny_rate: '8', bases: [{ model: 'custom', source_price_id: source.id, source_revision: source.revision, version: 3, price: { ...source, currency: 'USD', items: source.items.map(i => ({ ...i, amount: '0.2', unit: 'USD/MTok' })), conversion: { source_currency: 'CNY', usd_to_cny_rate: '7.15', source_items: source.items } } }] })
    const wrapper = render()
    await flushPromises()
    expect(wrapper.find('.effective-items').exists()).toBe(false)
    await wrapper.findAll('button').find(b => b.text() === '查看生效价')!.trigger('click')
    expect(wrapper.get('.effective-items strong').text()).toContain('0.2')
    expect(wrapper.get('.source-warning').text()).toContain('换算汇率已变化')
    expect(wrapper.get('.selection-footer button').attributes('disabled')).toBeUndefined()
    await wrapper.get('.selection-footer button').trigger('click')
    expect(wrapper.findAll('.confirmation-table .new-price').map(td => td.text())).toEqual(['0.25', '0.625'])
  })

  it('selects one complete price card including its revision', async () => {
    api.select.mockResolvedValue({})
    const wrapper = render()
    await flushPromises()
    expect(wrapper.text()).toContain('未定价')
    await wrapper.get('input[type="radio"]').setValue(true)
    await wrapper.findAll('button').find(b => b.text() === '设置基础价')!.trigger('click')
    expect(wrapper.text()).toContain('新基础价')
    await wrapper.findAll('button').find(b => b.text() === '保存基础价')!.trigger('click')
    await flushPromises()
    expect(api.select).toHaveBeenCalledWith('custom', expect.objectContaining({ id: 4, revision: 2 }), 0, undefined)
  })

  it('retains the selection and displays a version conflict', async () => {
    api.select.mockRejectedValue({ message: '价格已被修改，请刷新后重新确认' })
    const wrapper = render()
    await flushPromises()
    await wrapper.get('input[type="radio"]').setValue(true)
    await wrapper.findAll('button').find(b => b.text() === '设置基础价')!.trigger('click')
    await wrapper.findAll('button').find(b => b.text() === '保存基础价')!.trigger('click')
    await flushPromises()
    expect(wrapper.text()).toContain('价格已被修改，请刷新后重新确认')
    expect(wrapper.findAll('button').some(b => b.text() === '保存基础价')).toBe(true)
  })

  it('disables applying an unchanged source and enables an explicit source update', async () => {
    const fixture = await api.catalog()
    const price = fixture.accounts[0].prices[0]
    api.catalog.mockResolvedValue({ ...fixture, bases: [{ model: 'custom', source_price_id: price.id, source_revision: price.revision, version: 3, price: { ...price, items: price.items.map(i => ({ ...i })) } }] })
    const wrapper = render()
    await flushPromises()
    expect(wrapper.get('.selection-footer button').attributes('disabled')).toBeDefined()
    price.revision = 3
    api.catalog.mockResolvedValue({ ...fixture, bases: [{ model: 'custom', source_price_id: price.id, source_revision: 2, version: 3, price: { ...price, revision: 2 } }] })
    await wrapper.get('button[aria-label="刷新价格"]').trigger('click')
    await flushPromises()
    expect(wrapper.get('.selection-footer button').attributes('disabled')).toBeUndefined()
    await wrapper.get('.selection-footer button').trigger('click')
    await wrapper.findAll('button').find(b => b.text() === '保存基础价')!.trigger('click')
    await flushPromises()
    expect(api.select).toHaveBeenCalledWith('custom', expect.objectContaining({ id: 4, revision: 3 }), 3, undefined)
  })

  it('compares complete prices and shows removed cache items in confirmation', async () => {
    const fixture = await api.catalog()
    const price = fixture.accounts[0].prices[0]
    const old = { ...price, items: [...price.items, { kind: 'cache_read', tier: '', unit: 'USD/MTok', amount: '0' }] }
    api.catalog.mockResolvedValue({ ...fixture, bases: [{ model: 'custom', source_price_id: 99, source_revision: 1, version: 1, price: old }] })
    const wrapper = render()
    await flushPromises()
    await wrapper.get('input[type="radio"]').setValue(true)
    await wrapper.get('.selection-footer button').trigger('click')
    expect(wrapper.get('.confirmation-table').text()).toContain('缓存读取')
    expect(wrapper.get('.confirmation-table').text()).toContain('移除')
    expect(wrapper.get('.confirmation-table').text()).toContain('0')
  })

  it('keeps quote details and missing-account controls available outside the compact table', async () => {
    const fixture = await api.catalog()
    api.catalog.mockResolvedValue({ ...fixture, accounts: [...fixture.accounts, { ...fixture.accounts[0], id: 2, name: '未报价账号', prices: [] }] })
    const wrapper = render()
    await flushPromises()
    expect(wrapper.findAll('.comparison-table tbody tr')).toHaveLength(1)
    expect(wrapper.find('.confirmation-source').exists()).toBe(false)
    await wrapper.get('button[aria-label="账号 A 报价详情"]').trigger('click')
    expect(wrapper.get('.confirmation-source').text()).toContain('折扣 ×1 · 公司加价 ×1')
    expect(wrapper.get('.confirmation-table').text()).toContain('上游原价')
    await wrapper.get('button[aria-label="显示账号"]').trigger('click')
    expect(wrapper.get('.account-options').text()).toContain('未报价账号')
    expect(wrapper.findAll('.account-options input')[1].attributes('disabled')).toBeDefined()
    await wrapper.findAll('.account-options input')[0].setValue(false)
    expect(wrapper.find('.comparison-table').exists()).toBe(false)
    expect(wrapper.text()).toContain('暂无可比较的报价')
  })
})
