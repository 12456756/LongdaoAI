import { defineComponent, ref } from 'vue'
import { mount, flushPromises } from '@vue/test-utils'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { candidateAmount, configuredPrices, newModelPrice, type AccountPricingDraft } from '@/api/admin/modelPricing'
import AccountModelPricingEditor from '../AccountModelPricingEditor.vue'

const api = vi.hoisted(() => ({ companies: vi.fn(), accountPrices: vi.fn(), exchangeRate: vi.fn() }))
vi.mock('@/api/admin/modelPricing', async (original) => ({ ...await original<typeof import('@/api/admin/modelPricing')>(), modelPricingAPI: api }))

describe('account model pricing', () => {
  beforeEach(() => {
    api.exchangeRate.mockResolvedValue({ usd_to_cny_rate: '7.15' })
    api.companies.mockResolvedValue([{ id: 1, name: '上游 A', notes: '' }])
    api.accountPrices.mockResolvedValue({ prices: [], version: 'v1' })
  })
  afterEach(() => { vi.restoreAllMocks() })

  function renderPrice(price = newModelPrice('custom')) {
    const draft = ref<AccountPricingDraft>({ companyId: 1, prices: [price], loaded: false })
    const wrapper = mount(defineComponent({
      components: { AccountModelPricingEditor }, setup: () => ({ draft }),
      template: '<AccountModelPricingEditor v-model="draft" />'
    }), { global: { stubs: { Icon: true } } })
    return { draft, wrapper }
  }

  it('loads every existing whitelist model without mappings and preserves saved quotes', async () => {
    const saved = newModelPrice('model-a')
    saved.id = 10
    saved.items[0].amount = '2'
    saved.items[1].amount = '8'
    let finishLoad!: (value: { prices: typeof saved[]; version: string }) => void
    api.accountPrices.mockReturnValue(new Promise(resolve => { finishLoad = resolve }))
    const draft = ref<AccountPricingDraft>({ companyId: 1, prices: [], loaded: false })
    const whitelist = ref(['model-a'])
    const wrapper = mount(defineComponent({
      components: { AccountModelPricingEditor }, setup: () => ({ draft, whitelist }),
      template: '<AccountModelPricingEditor v-model="draft" :account-id="1" :restricted-models="whitelist" />'
    }), { global: { stubs: { Icon: true } } })
    whitelist.value = ['model-a', 'model-b', 'model-c', 'model-b', 'model-*']
    await flushPromises()
    finishLoad({ prices: [saved], version: 'v2' })
    await flushPromises()
    expect(draft.value.prices.map(price => price.model)).toEqual(['model-a', 'model-b', 'model-c'])
    expect(draft.value.prices[0].items[0].amount).toBe('2')
    expect(draft.value.prices[1].upstream_model).toBe('model-b')
    expect(configuredPrices(draft.value.prices)).toHaveLength(1)
    expect(wrapper.text()).toContain('白名单模型 3 个 · 已填写 1 个')
    expect(wrapper.find('input[aria-label="上游模型"]').exists()).toBe(false)
    await wrapper.findAll('.quote-model')[1].trigger('click')
    await wrapper.get('input[aria-label="model-b input 原价"]').setValue('3')
    whitelist.value = [...whitelist.value, 'model-d']
    await flushPromises()
    expect(draft.value.prices.map(price => price.model)).toEqual(['model-a', 'model-b', 'model-c', 'model-d'])
    expect(draft.value.prices[1].items[0].amount).toBe('3')
  })

  it('seeds a new account from its initial whitelist before any selection changes', async () => {
    const draft = ref<AccountPricingDraft>({ companyId: 0, prices: [], loaded: false })
    const wrapper = mount(defineComponent({
      components: { AccountModelPricingEditor }, setup: () => ({ draft }),
      template: '<AccountModelPricingEditor v-model="draft" :restricted-models="[\'alpha\', \'beta\']" />'
    }), { global: { stubs: { Icon: true } } })
    await flushPromises()
    expect(draft.value.prices.map(price => price.model)).toEqual(['alpha', 'beta'])
    expect(wrapper.findAll('.quote-model')).toHaveLength(2)
    expect(configuredPrices(draft.value.prices)).toEqual([])
  })

  it('searches and filters without editing a different model or dropping another draft', async () => {
    const draft = ref<AccountPricingDraft>({ companyId: 0, prices: [], loaded: false })
    const wrapper = mount(defineComponent({
      components: { AccountModelPricingEditor }, setup: () => ({ draft }),
      template: '<AccountModelPricingEditor v-model="draft" :models="[\'alpha\', \'beta\', \'gamma\']" />'
    }), { global: { stubs: { Icon: true } } })
    await flushPromises()
    await wrapper.get('input[aria-label="搜索报价模型"]').setValue('GAM')
    expect(wrapper.findAll('.quote-model')).toHaveLength(1)
    expect(wrapper.get('.editor-title h4').text()).toBe('gamma')
    await wrapper.get('input[aria-label="gamma input 原价"]').setValue('0')
    await wrapper.get('input[aria-label="gamma output 原价"]').setValue('2')
    expect(draft.value.prices[2].items[0].amount).toBe('0')
    expect(draft.value.prices[0].items[0].amount).toBe('')
    await wrapper.get('button[aria-label="清空模型搜索"]').trigger('click')
    await wrapper.findAll('.model-filters button')[1].trigger('click')
    expect(wrapper.findAll('.quote-model').map(row => row.find('.model-name').text())).toEqual(['alpha', 'beta'])
    await wrapper.findAll('.model-filters button')[2].trigger('click')
    expect(wrapper.findAll('.quote-model')).toHaveLength(1)
    expect(wrapper.get('.editor-title h4').text()).toBe('gamma')
    expect((wrapper.get('input[aria-label="gamma output 原价"]').element as HTMLInputElement).value).toBe('2')
  })

  it('reports unsaved changes and removals against the loaded quote, including reverting edits', async () => {
    const saved = newModelPrice('saved')
    saved.id = 12
    saved.items[0].amount = '1'; saved.items[1].amount = '2'
    api.accountPrices.mockResolvedValue({ prices: [saved], version: 'saved-version' })
    const draft = ref<AccountPricingDraft>({ companyId: 0, prices: [], loaded: false })
    const wrapper = mount(defineComponent({
      components: { AccountModelPricingEditor }, setup: () => ({ draft }),
      template: '<AccountModelPricingEditor v-model="draft" :account-id="1" />'
    }), { global: { stubs: { Icon: true } } })
    await flushPromises()
    const editor = wrapper.findComponent(AccountModelPricingEditor)
    expect(editor.emitted('dirty-count')?.slice(-1)[0]).toEqual([0])
    await wrapper.get('input[aria-label="saved input 原价"]').setValue('3')
    expect(editor.emitted('dirty-count')?.slice(-1)[0]).toEqual([1])
    await wrapper.get('input[aria-label="saved input 原价"]').setValue('1')
    expect(editor.emitted('dirty-count')?.slice(-1)[0]).toEqual([0])
    vi.spyOn(window, 'confirm').mockReturnValue(true)
    await wrapper.get('button[aria-label="移除模型报价"]').trigger('click')
    expect(draft.value.prices).toHaveLength(0)
    expect(editor.emitted('dirty-count')?.slice(-1)[0]).toEqual([1])
  })

  it('keeps token input and output fixed and distinguishes optional zero from missing', async () => {
    const { draft, wrapper } = renderPrice()
    await flushPromises()
    expect(wrapper.find('select[aria-label="报价币种"]').exists()).toBe(true)
    const optional = wrapper.get('input[aria-label="custom cache_read 原价"]')
    await optional.setValue('0')
    expect(draft.value.prices[0].items.find(i => i.kind === 'cache_read')?.amount).toBe('0')
    await optional.setValue('')
    expect(draft.value.prices[0].items.some(i => i.kind === 'cache_read')).toBe(false)
    expect(draft.value.prices[0].items.map(i => i.kind)).toEqual(['input', 'output'])
  })

  it('replaces obsolete empty model rows and keeps the selected quote and entered prices', async () => {
    const saved = newModelPrice('saved')
    saved.id = 7
    const draft = ref<AccountPricingDraft>({ companyId: 0, prices: [saved], loaded: false })
    const whitelist = ref(['old-empty', 'edited', 'selected'])
    const wrapper = mount(defineComponent({
      components: { AccountModelPricingEditor }, setup: () => ({ draft, whitelist }),
      template: '<AccountModelPricingEditor v-model="draft" :restricted-models="whitelist" />'
    }), { global: { stubs: { Icon: true } } })
    await flushPromises()
    draft.value.prices.find(price => price.model === 'edited')!.items[0].amount = '0'
    await wrapper.findAll('.quote-model')[3].trigger('click')
    whitelist.value = ['selected', ...Array.from({ length: 65 }, (_, i) => `model-${i}`)]
    await flushPromises()
    expect(draft.value.prices).toHaveLength(68)
    expect(draft.value.prices.some(price => price.model === 'old-empty')).toBe(false)
    expect(draft.value.prices.find(price => price.model === 'edited')?.items[0].amount).toBe('0')
    expect(wrapper.get('.editor-title h4').text()).toBe('selected')
    whitelist.value = []
    await flushPromises()
    expect(draft.value.prices.map(price => price.model)).toEqual(['saved', 'edited'])
  })

  it('shows the model as a heading and retains its saved upstream alias while editing prices', async () => {
    const price = newModelPrice('custom')
    price.upstream_model = 'saved-upstream-alias'
    const { draft, wrapper } = renderPrice(price)
    await flushPromises()
    expect(wrapper.get('.editor-title h4').text()).toBe('custom')
    expect(wrapper.find('input[aria-label="对外模型"]').exists()).toBe(false)
    expect(wrapper.find('input[aria-label="上游模型"]').exists()).toBe(false)
    await wrapper.get('input[aria-label="custom input 原价"]').setValue('2')
    expect(draft.value.prices[0].model).toBe('custom')
    expect(draft.value.prices[0].upstream_model).toBe('saved-upstream-alias')
  })

  it('preserves entered prices when a billing mode change is cancelled', async () => {
    const price = newModelPrice('custom')
    price.items[0].amount = '2'
    const { draft, wrapper } = renderPrice(price)
    await flushPromises()
    const confirm = vi.spyOn(window, 'confirm').mockReturnValue(false)
    await wrapper.get('button[aria-pressed][type="button"]:nth-child(2)').trigger('click')
    expect(confirm).toHaveBeenCalledOnce()
    expect(draft.value.prices[0].mode).toBe('token')
    expect(draft.value.prices[0].items[0].amount).toBe('2')
    confirm.mockReturnValue(true)
    await wrapper.get('button[aria-pressed][type="button"]:nth-child(2)').trigger('click')
    expect(draft.value.prices[0].mode).toBe('image')
    expect(wrapper.find('input[aria-label="custom input 原价"]').exists()).toBe(false)
    await wrapper.get('input[aria-label="custom image 2K 原价"]').setValue('0.04')
    expect(draft.value.prices[0].items).toEqual([{ kind: 'image', tier: '2K', unit: 'USD/image', amount: '0.04' }])
  })

  it('shows the selected model without losing another model draft', async () => {
    const draft = ref<AccountPricingDraft>({ companyId: 1, prices: [newModelPrice('custom')], loaded: false })
    const models = ref(['custom'])
    const wrapper = mount(defineComponent({
      components: { AccountModelPricingEditor }, setup: () => ({ draft, models }),
      template: '<AccountModelPricingEditor v-model="draft" :models="models" />'
    }), { global: { stubs: { Icon: true } } })
    await flushPromises()
    expect(wrapper.find('input[aria-label="选择或输入模型名称"]').exists()).toBe(false)
    await wrapper.get('input[aria-label="custom input 原价"]').setValue('3')
    models.value.push('second-model')
    await flushPromises()
    await wrapper.findAll('.quote-model')[1].trigger('click')
    expect(wrapper.find('input[aria-label="second-model input 原价"]').exists()).toBe(true)
    expect(draft.value.prices[0].items[0].amount).toBe('3')
    await wrapper.get('.quote-model').trigger('click')
    expect((wrapper.get('input[aria-label="custom input 原价"]').element as HTMLInputElement).value).toBe('3')
    expect(wrapper.get('a').attributes('target')).toBe('_blank')
  })

  it('keeps exact decimal prices as strings when edited and previewed', async () => {
    const price = newModelPrice('custom')
    price.discount = '0.1'; price.markup = '1.2'
    const draft = ref<AccountPricingDraft>({ companyId: 1, prices: [price], loaded: false })
    const wrapper = mount(defineComponent({
      components: { AccountModelPricingEditor }, setup: () => ({ draft }),
      template: '<AccountModelPricingEditor v-model="draft" />'
    }), { global: { stubs: { Icon: true } } })
    await flushPromises()
    await wrapper.get('input[aria-label="custom input 原价"]').setValue('0.3')
    expect(draft.value.prices[0].items[0].amount).toBe('0.3')
    expect(candidateAmount(draft.value.prices[0].items[0], draft.value.prices[0])).toBe('0.036')
    expect(wrapper.text()).toContain('0.036')
    expect(draft.value.loaded).toBe(true)
  })

  it('does not permit a failed read to erase saved prices', async () => {
    api.accountPrices.mockRejectedValue({ message: '价格读取失败' })
    const draft = { companyId: 1, prices: [], loaded: false }
    const wrapper = mount(AccountModelPricingEditor, { props: { modelValue: draft, accountId: 1 }, global: { stubs: { Icon: true } } })
    await flushPromises()
    expect(wrapper.get('[role="alert"]').text()).toContain('价格读取失败')
    expect(wrapper.emitted('update:modelValue')).toBeUndefined()
    expect(draft.loaded).toBe(false)
  })

  it('distinguishes blank values from explicitly free prices', () => {
    const price = newModelPrice()
    expect(candidateAmount(price.items[0], price)).toBe('-')
    expect(configuredPrices([price])).toHaveLength(0)
    price.items[0].amount = '0'
    expect(configuredPrices([price])).toHaveLength(1)
    expect(candidateAmount(price.items[0], price)).toBe('0')
    price.items[0].amount = '0.123456789012'
    price.discount = '0.123456789012'; price.markup = '0.123456789012'
    expect(candidateAmount(price.items[0], price)).toBe('0.001881676372337851695957261088849728')
  })

  it('confirms currency reinterpretation and uses yuan for optional cache and image fields', async () => {
    const price = newModelPrice('custom')
    price.items[0].amount = '1.43'; price.items[1].amount = '7.15'
    const { draft, wrapper } = renderPrice(price)
    await flushPromises()
    const confirm = vi.spyOn(window, 'confirm').mockReturnValue(false)
    await wrapper.get('select[aria-label="报价币种"]').setValue('CNY')
    expect(draft.value.prices[0].currency).toBe('USD')
    expect((wrapper.get('select[aria-label="报价币种"]').element as HTMLSelectElement).value).toBe('USD')
    confirm.mockReturnValue(true)
    await wrapper.get('select[aria-label="报价币种"]').setValue('USD')
    await wrapper.get('select[aria-label="报价币种"]').setValue('CNY')
    expect(draft.value.prices[0].items[0]).toMatchObject({ amount: '1.43', unit: 'CNY/MTok' })
    expect(wrapper.text()).toContain('0.2')
    await wrapper.get('input[aria-label="custom cache_read 原价"]').setValue('0')
    expect(draft.value.prices[0].items.find(i => i.kind === 'cache_read')).toMatchObject({ amount: '0', unit: 'CNY/MTok' })
    await wrapper.get('button[aria-pressed][type="button"]:nth-child(2)').trigger('click')
    await wrapper.get('input[aria-label="custom image 2K 原价"]').setValue('1.43')
    expect(draft.value.prices[0].items[0]).toMatchObject({ amount: '1.43', unit: 'CNY/image' })
  })
})
