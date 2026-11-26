import { flushPromises, mount } from '@vue/test-utils'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import SEPIntegrationView from '../SEPIntegrationView.vue'

const api = vi.hoisted(() => ({ config: vi.fn(), save: vi.fn(), enterprises: vi.fn(), detail: vi.fn(), update: vi.fn(), ledger: vi.fn(), groups: vi.fn(), events: vi.fn(), retryEvent: vi.fn(), eventSigningKey: vi.fn() }))
vi.mock('vue-router', () => ({ useRoute: () => ({ query: {} }) }))
vi.mock('@/api/admin/sep', () => ({ sepAPI: api }))
vi.mock('@/api/admin/groups', () => ({ getAll: api.groups, default: { getAll: api.groups } }))
vi.mock('@/stores/app', () => ({ useAppStore: () => ({ balanceDisplayCnyRate: 7.15 }) }))

describe('SEP integration administration', () => {
  beforeEach(() => {
    vi.clearAllMocks()
    const config = { enabled: false, integration_id: 'sep-test', signing_key_id: 'sign-v1', signing_public_key: 'public-sign', encryption_key_id: 'encrypt-v1', encryption_public_key: 'public-encrypt', chat_group_id: 1, image_group_id: 2, video_group_id: 2 }
    api.config.mockResolvedValue(config)
    api.save.mockImplementation(async value => value)
    api.enterprises.mockResolvedValue([])
    api.events.mockResolvedValue({ items: [], after_sequence: '0', has_more: false })
    api.groups.mockResolvedValue([{ id: 1, name: 'Chat', platform: 'openai', status: 'active', subscription_type: 'standard', allow_image_generation: false, rate_multiplier: 1 }, { id: 2, name: 'Media', platform: 'openai', status: 'active', subscription_type: 'standard', allow_image_generation: true, rate_multiplier: 1.5 }, { id: 3, name: 'Subscription', platform: 'openai', status: 'active', subscription_type: 'subscription', allow_image_generation: true }, { id: 4, name: 'Disabled', platform: 'openai', status: 'disabled', subscription_type: 'standard', allow_image_generation: true }])
  })
  const render = () => mount(SEPIntegrationView, { global: { stubs: { AppLayout: { template: '<main><slot /></main>' }, BaseDialog: { props: ['show'], template: '<div v-if="show"><slot /></div>' }, Icon: true, RouterLink: { template: '<a><slot /></a>' } } } })
  it('filters media and subscription groups and saves the three explicit defaults', async () => {
    const wrapper = render(); await flushPromises()
    expect(wrapper.get('select[aria-label="对话 Key"]').findAll('option').map(o => o.text())).toEqual(['选择分组', 'Chat · openai · 1x', 'Media · openai · 1.5x'])
    expect(wrapper.get('select[aria-label="图片 Key"]').findAll('option').map(o => o.text())).toEqual(['选择分组', 'Media · openai · 1.5x'])
    expect(wrapper.text()).toContain('1 USD = 7.15 CNY')
    await wrapper.find('form').trigger('submit.prevent'); await flushPromises()
    expect(api.save).toHaveBeenCalledWith(expect.objectContaining({ chat_group_id: 1, image_group_id: 2, video_group_id: 2 }))
    expect(wrapper.text()).toContain('配置已保存')
  })
  it('shows a failed load and supports recovery through refresh', async () => {
    api.config.mockRejectedValueOnce({ message: 'database unavailable' })
    const wrapper = render(); await flushPromises()
    expect(wrapper.get('[role="alert"]').text()).toContain('database unavailable')
    await wrapper.findAll('button').find(b => b.text() === '刷新')!.trigger('click'); await flushPromises()
    expect(wrapper.find('[role="alert"]').exists()).toBe(false)
  })
  it('keeps empty enterprise state explicit', async () => {
    const wrapper = render(); await flushPromises()
    await wrapper.get('[role="tablist"]').findAll('button')[1].trigger('click')
    expect(wrapper.text()).toContain('暂无已开户企业')
    await wrapper.get('input[type="search"]').setValue('missing')
    expect(wrapper.text()).toContain('没有匹配的企业')
  })
  it('shows the exact remote rejection reason and retries the event without recharging', async () => {
    api.enterprises.mockResolvedValue([{ enterprise_id: 'enterprise', company_name: 'Company', admin_email: 'admin@example.test', compute_account_id: 1, status: 'ready' }])
    api.detail.mockResolvedValue({ enterprise: { enterprise_id: 'enterprise', company_name: 'Company', credentials: [], status: 'ready' }, balance: { available_paid_cny: '1.000000', frozen_paid_cny: '0', sequence: '1' }, bills: [] })
    api.ledger.mockResolvedValue({ items: [], has_more: false })
    api.events.mockResolvedValue({ items: [{ event_id: 'event-1', sequence: '1', status: 'failed', attempts: 2, last_error: 'HTTP 409：enterprise binding missing', payload: { origin: 'compute', delta_cny: '1.000000' } }], has_more: false })
    const wrapper = render(); await flushPromises()
    await wrapper.get('[role="tablist"]').findAll('button')[1].trigger('click')
    await wrapper.findAll('button').find(b => b.text() === '查看')!.trigger('click'); await flushPromises()
    expect(wrapper.text()).toContain('HTTP 409：enterprise binding missing')
    await wrapper.findAll('button').find(b => b.text() === '重试')!.trigger('click'); await flushPromises()
    expect(api.retryEvent).toHaveBeenCalledWith('event-1')
  })
})
