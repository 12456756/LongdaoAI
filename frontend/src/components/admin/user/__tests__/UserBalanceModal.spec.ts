import { flushPromises, mount } from '@vue/test-utils'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import UserBalanceModal from '../UserBalanceModal.vue'
import type { AdminUser } from '@/types'

const api = vi.hoisted(() => ({ updateBalance: vi.fn(), showSuccess: vi.fn(), showError: vi.fn() }))
vi.mock('@/api/admin', () => ({ adminAPI: { users: { updateBalance: api.updateBalance } } }))
vi.mock('@/stores/app', () => ({ useAppStore: () => api }))
vi.mock('vue-i18n', () => ({ useI18n: () => ({ t: (key: string) => key }) }))
describe('SEP enterprise balance adjustment', () => {
  beforeEach(() => { vi.clearAllMocks() })
  const user = { id: 1, email: 'enterprise@example.test', balance: 2, sep_balance: { enterprise_id: 'enterprise', available_paid_cny: '14.300000', frozen_paid_cny: '0.000000', exchange_rate: '7.15' } } as AdminUser
  const render = (value = user) => mount(UserBalanceModal, { props: { show: false, user: value, operation: 'add' }, global: { stubs: { BaseDialog: { template: '<div><slot /><slot name="footer" /></div>' } } } })
  it('uses the enterprise yuan balance and retains its operation ID across failed retries', async () => {
    api.updateBalance.mockRejectedValue({ message: '数据库连接超时，未入账', reason: 'SEP_DATABASE_TIMEOUT', metadata: { request_id: 'trace-1' } })
    const wrapper = render(); await wrapper.setProps({ show: true })
    expect(wrapper.text()).toContain('¥14.30')
    await wrapper.get('input[type="number"]').setValue('1')
    await wrapper.get('form').trigger('submit.prevent'); await flushPromises()
    expect(wrapper.get('[role="alert"]').text()).toContain('数据库连接超时，未入账')
    expect(wrapper.get('[role="alert"]').text()).toContain('SEP_DATABASE_TIMEOUT')
    expect(wrapper.get('[role="alert"]').text()).toContain('trace-1')
    const key = api.updateBalance.mock.calls[0][4]
    expect(key).toBeTruthy()
    await wrapper.get('form').trigger('submit.prevent'); await flushPromises()
    expect(api.updateBalance.mock.calls[1][4]).toBe(key)
    expect(api.updateBalance.mock.calls[0].slice(0, 4)).toEqual([1, 1, 'add', ''])
  })
  it('labels local posting as committed without claiming SEP has received it', async () => {
    api.updateBalance.mockResolvedValue({ sep_balance_receipt: { committed: true, sync_status: 'pending' } })
    const wrapper = render(); await wrapper.setProps({ show: true })
    await wrapper.get('input[type="number"]').setValue('1')
    await wrapper.get('form').trigger('submit.prevent'); await flushPromises()
    expect(api.showSuccess).toHaveBeenCalledWith('余额已入账，SEP 同步事件已保存')
    expect(wrapper.emitted('success')).toHaveLength(1)
  })
  it('keeps ordinary users in dollars and uses their existing balance', async () => {
    api.updateBalance.mockResolvedValue({})
    const wrapper = render({ ...user, sep_balance: undefined })
    await wrapper.setProps({ show: true })
    expect(wrapper.text()).toContain('$2.00')
    await wrapper.get('input[type="number"]').setValue('1')
    await wrapper.get('form').trigger('submit.prevent'); await flushPromises()
    expect(api.updateBalance).toHaveBeenCalledWith(1, 1, 'add', '', undefined)
  })
})
