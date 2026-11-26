<template>
  <AppLayout class="pricing-layout">
    <div class="pricing-page">
      <header class="page-heading">
        <div class="flex min-w-0 items-center gap-3"><h1>模型基础价格</h1><span class="count-label">{{ totalRows }} 个模型</span></div>
        <div class="heading-actions">
          <a href="/admin/accounts" class="btn btn-secondary" title="账号管理" aria-label="账号管理"><Icon name="externalLink" size="sm" /><span>账号管理</span></a>
          <button class="btn btn-secondary" title="上游公司" aria-label="上游公司" @click="companyDialog = true"><Icon name="globe" size="sm" /><span>上游公司</span></button>
          <button class="icon-button" title="刷新价格" aria-label="刷新价格" :disabled="loading" @click="load"><Icon name="refresh" size="sm" :class="{ 'animate-spin': loading }" /></button>
        </div>
      </header>

      <div :class="['pricing-toolbar', { 'filters-open': filtersOpen }]">
        <div class="search-field"><Icon name="search" size="sm" /><input v-model="search" type="search" class="input" placeholder="搜索模型" aria-label="搜索模型" /></div>
        <button class="icon-button mobile-filter-toggle" title="筛选模型" aria-label="筛选模型" :aria-expanded="filtersOpen" aria-controls="pricing-filters" @click="filtersOpen = !filtersOpen"><Icon name="filter" size="sm" /></button>
        <div id="pricing-filters" class="pricing-filters">
        <select v-model="companyFilter" class="input" aria-label="筛选上游公司"><option value="">全部上游公司</option><option v-for="company in companies" :key="company.id" :value="String(company.id)">{{ company.name }}</option><option value="0">未归属</option></select>
        <select v-model="platformFilter" class="input" aria-label="筛选平台"><option value="">全部平台</option><option v-for="platform in platforms" :key="platform" :value="platform">{{ platform }}</option></select>
        <select v-model="modeFilter" class="input" aria-label="筛选计费类型"><option value="">全部计费类型</option><option value="token">Token</option><option value="image">图片</option><option value="video">视频</option><option value="per_request">按次</option></select>
        </div>
      </div>
      <div class="status-tabs" role="tablist" aria-label="定价状态">
        <button v-for="status in statuses" :key="status.value" role="tab" :aria-selected="statusFilter === status.value" :class="{ active: statusFilter === status.value }" @click="statusFilter = status.value">{{ status.label }}</button>
      </div>
      <p v-if="error && !pending && !companyDialog && !historyModel" class="feedback error-feedback" role="alert">{{ error }}<button class="underline" @click="load">重试</button></p>
      <p v-if="success" class="feedback success-feedback" role="status"><Icon name="check" size="sm" />{{ success }}</p>

      <div class="pricing-workspace" :aria-busy="loading">
        <aside class="model-list" aria-label="模型列表">
          <div class="list-heading"><span>模型</span><span>{{ loading ? '读取中' : `${totalRows} 项` }}</span></div>
          <select class="input mobile-model-picker" aria-label="选择模型" :value="activeModel" :disabled="loading || !pagedRows.length" @change="chooseModel(($event.target as HTMLSelectElement).value)"><option v-if="!pagedRows.length" value="">{{ loading ? '正在读取...' : '没有匹配的模型' }}</option><option v-for="row in pagedRows" :key="row.model" :value="row.model">{{ row.model }} · {{ rowStatus(row) }}</option></select>
          <div class="model-list-content">
            <button v-for="row in pagedRows" :key="row.model" :class="['model-option', { active: activeModel === row.model }]" :aria-current="activeModel === row.model ? 'true' : undefined" @click="chooseModel(row.model)">
              <span class="model-name">{{ row.model }}</span>
              <span class="model-option-meta"><span>{{ rowMode(row) }}</span><span :class="['price-status', statusClass(row)]">{{ rowStatus(row) }}</span></span>
            </button>
            <p v-if="!pagedRows.length" class="list-empty">{{ loading ? '正在读取...' : '没有匹配的模型' }}</p>
          </div>
          <div class="list-pagination"><button class="icon-button" title="上一页" aria-label="上一页" :disabled="page <= 1 || loading" @click="page--"><Icon name="chevronLeft" size="sm" /></button><span>{{ page }} / {{ pageCount }}</span><button class="icon-button" title="下一页" aria-label="下一页" :disabled="page >= pageCount || loading" @click="page++"><Icon name="chevronRight" size="sm" /></button></div>
        </aside>

        <section v-if="activeRow" class="model-detail">
          <header class="detail-heading"><div class="min-w-0"><h2>{{ activeRow.model }}</h2><span :class="['price-status', statusClass(activeRow)]">{{ rowStatus(activeRow) }}</span></div><div class="detail-actions"><button v-if="activeRow.base" class="history-button" @click="effectiveDialog = true"><Icon name="eye" size="sm" />查看生效价</button><button v-if="activeRow.base" class="icon-button" title="价格历史" aria-label="价格历史" @click="showHistory(activeRow.model)"><Icon name="clock" size="sm" /></button></div></header>
          <div class="effective-summary" aria-label="当前生效基础价">
            <template v-if="activeRow.base"><span class="summary-label">当前来源</span><strong :title="sourceName(activeRow.base)">{{ sourceName(activeRow.base) }}</strong><span class="version">v{{ activeRow.base.version }}</span></template>
            <template v-else><span class="price-status missing">尚未设置基础价</span><span class="summary-label">暂不可计费调用</span></template>
          </div>
          <p v-if="activeRow.base && sourceChanged(activeRow.base)" class="source-warning"><Icon name="exclamationTriangle" size="sm" />来源报价或换算汇率已变化，当前仍按已确认的基础价计费。</p>

          <div class="comparison-heading"><h3>账号报价对比<span>{{ candidateAccounts.length }} 个报价</span></h3>
            <div class="comparison-actions"><button v-if="unpricedAccounts.length" class="missing-quotes" @click="accountFilterDialog = true">{{ unpricedAccounts.length }} 个账号未报价</button><button class="icon-button" title="显示账号" aria-label="显示账号" @click="accountFilterDialog = true"><Icon name="filter" size="sm" /></button></div>
          </div>
          <div v-if="candidateAccounts.length" class="comparison-scroll" tabindex="0" aria-label="账号报价对比表">
            <table class="comparison-table">
              <thead>
                <tr><th scope="col" class="account-column">上游公司 / 账号</th><th v-for="item in comparisonItems" :key="itemKey(item)" scope="col" class="price-column">{{ itemLabel(item) }}<span>{{ unitLabel(usdUnit(item.unit)) }}</span></th></tr>
              </thead>
              <tbody>
                <tr v-for="account in candidateAccounts" :key="account.id" :class="{ chosen: selected[activeRow.model] === findPrice(account, activeRow.model)!.id }">
                  <th scope="row" class="account-column">
                    <div class="account-identity"><label class="select-source"><input v-model="selected[activeRow.model]" type="radio" :name="`source-${activeRow.model}`" :value="findPrice(account, activeRow.model)!.id" :disabled="loading" :aria-label="`${activeRow.model} 选择 ${account.company_name || '未归属'} ${account.name}`" /><span class="account-name">{{ account.name }}</span></label><button class="quote-detail-button" :title="`${account.name} 报价详情`" :aria-label="`${account.name} 报价详情`" @click="quoteAccount = account"><Icon name="more" size="sm" /></button></div>
                    <span class="account-company">{{ account.company_name || '未归属' }}</span>
                    <span class="account-meta"><span v-if="isEffectiveSource(account)" class="effective-source">当前来源</span><span v-if="account.status !== 'active'" class="text-amber-600">已停用</span><span>{{ findPrice(account, activeRow.model)!.currency || 'USD' }} 原报价</span></span>
                  </th>
                  <td v-for="item in comparisonItems" :key="itemKey(item)" class="price-column"><strong class="quote-amount" :title="comparisonAmount(account, item)">{{ formatPriceAmount(comparisonAmount(account, item)) }}</strong><span v-if="comparisonAmount(account, item) !== '未配置'" class="original-price">≈ ¥{{ cnyReference(comparisonAmount(account, item)) }}</span></td>
                </tr>
              </tbody>
            </table>
          </div>
          <div v-else class="no-quotes"><Icon name="calculator" size="lg" /><span>{{ supportedAccounts.length ? '暂无可比较的报价' : '暂无支持该模型的账号' }}</span><a href="/admin/accounts" class="pricing-link">去账号管理配置报价<Icon name="arrowRight" size="sm" /></a></div>

          <footer class="selection-footer"><div class="selection-summary"><span>{{ selectedAccount && !canApply ? '当前来源' : '待生效来源' }}</span><strong>{{ selectedAccount ? `${selectedAccount.company_name || '未归属'} / ${selectedAccount.name}` : '尚未选择' }}</strong></div>
            <button class="btn btn-primary" :disabled="!canApply || loading || !!saving" @click="preview(activeRow)"><Icon name="check" size="sm" />{{ activeRow.base ? '更新基础价' : '设置基础价' }}</button>
          </footer>
        </section>
        <div v-else class="empty-workspace"><Icon name="search" size="lg" /><span>{{ loading ? '正在读取模型价格...' : '没有匹配的模型' }}</span><a v-if="!loading" href="/admin/accounts" class="pricing-link">账号管理<Icon name="arrowRight" size="sm" /></a></div>
      </div>
    </div>

    <BaseDialog :show="effectiveDialog && !!activeRow?.base" title="当前生效基础价格" width="wide" @close="effectiveDialog = false">
      <template v-if="activeRow?.base"><div class="confirmation-source"><span>{{ activeRow.model }} · v{{ activeRow.base.version }}</span><strong>{{ sourceName(activeRow.base) }}</strong><small v-if="activeRow.base.updated_at">生效时间 {{ new Date(activeRow.base.updated_at).toLocaleString() }}</small></div>
        <div class="confirmation-scroll"><table class="confirmation-table effective-items"><thead><tr><th>价格项目</th><th>生效基础价</th></tr></thead><tbody><tr v-for="item in activeRow.base.price.items" :key="itemKey(item)"><td>{{ itemLabel(item) }}<small>{{ unitLabel(item.unit) }}</small></td><td><strong :title="item.amount">{{ formatPriceAmount(item.amount) }}</strong></td></tr></tbody></table></div>
        <p v-if="activeRow.base.price.conversion" class="policy-line">原报价 {{ activeRow.base.price.conversion.source_currency }} · 生效汇率 1 USD = {{ activeRow.base.price.conversion.usd_to_cny_rate }} CNY</p><p v-if="activeRow.base.price.policy.long_context_threshold" class="policy-line">{{ policyLabel(activeRow.base.price) }}</p>
      </template>
    </BaseDialog>
    <BaseDialog :show="!!quoteAccount" title="账号报价详情" width="wide" @close="quoteAccount = null">
      <template v-if="quoteAccount && quotePrice"><div class="confirmation-source"><span>{{ quoteAccount.company_name || '未归属' }} / {{ quoteAccount.name }}</span><strong>{{ activeModel }} · {{ modeLabel(quotePrice.mode) }}</strong><small>原报价 {{ quotePrice.currency || 'USD' }} · 折扣 ×{{ quotePrice.discount }} · 公司加价 ×{{ quotePrice.markup }}</small></div>
        <div class="confirmation-scroll"><table class="confirmation-table"><thead><tr><th>价格项目</th><th>上游原价 · {{ quotePrice.currency || 'USD' }}</th><th>候选基础价 · USD</th></tr></thead><tbody><tr v-for="item in quotePrice.items" :key="itemKey(item)"><td>{{ itemLabel(item) }}<small>{{ unitLabel(item.unit) }}</small></td><td>{{ formatPriceAmount(item.amount) }}</td><td class="new-price">{{ formatPriceAmount(comparisonAmount(quoteAccount, item)) }}<small>≈ ¥{{ cnyReference(comparisonAmount(quoteAccount, item)) }}</small></td></tr></tbody></table></div><p v-if="quotePrice.policy.long_context_threshold" class="policy-line">{{ policyLabel(quotePrice) }}</p>
      </template>
    </BaseDialog>
    <BaseDialog :show="accountFilterDialog" title="显示账号" width="normal" @close="accountFilterDialog = false"><div class="account-options"><label v-for="account in supportedAccounts" :key="account.id"><input type="checkbox" :checked="!hiddenAccounts.includes(account.id)" :disabled="!findPrice(account, activeModel)" @change="toggleAccount(account.id)" /><span>{{ account.company_name || '未归属' }} / {{ account.name }}<small v-if="!findPrice(account, activeModel)">未配置报价</small></span></label><p v-if="!supportedAccounts.length" class="list-empty">暂无支持该模型的账号</p></div></BaseDialog>
    <BaseDialog :show="!!pending" title="确认模型基础价格" width="wide" :close-on-escape="!saving" :show-close-button="!saving" @close="pending = null">
      <template v-if="pending"><div class="confirmation-source"><span>{{ pending.model }}</span><strong>{{ pending.account.company_name || '未归属' }} / {{ pending.account.name }}</strong><small>原报价 {{ pending.price.currency || 'USD' }} · 确认汇率 1 USD = {{ catalog.usd_to_cny_rate }} CNY</small></div>
        <div class="confirmation-scroll"><table class="confirmation-table"><thead><tr><th>价格项目</th><th>当前基础价 · USD</th><th>新基础价 · USD</th></tr></thead><tbody><tr v-for="item in confirmationItems" :key="itemKey(item)"><td>{{ itemLabel(item) }}<small>{{ unitLabel(usdUnit(item.unit)) }}</small></td><td :title="currentAmount(pending.base, item)">{{ formatPriceAmount(currentAmount(pending.base, item)) }}</td><td class="new-price" :title="newAmount(pending.price, item)">{{ formatPriceAmount(newAmount(pending.price, item)) }}</td></tr><tr v-if="pending.price.policy.long_context_threshold || pending.base?.price.policy.long_context_threshold"><td>长上下文</td><td>{{ pending.base ? policyLabel(pending.base.price) || '未启用' : '未启用' }}</td><td>{{ policyLabel(pending.price) || '未启用' }}</td></tr></tbody></table></div>
        <p v-if="error" class="mt-3 text-sm text-red-600" role="alert">{{ error }}</p>
      </template>
      <template #footer><button class="icon-button" title="刷新价格并重新选择" aria-label="刷新价格并重新选择" :disabled="!!saving" @click="refreshComparison"><Icon name="refresh" size="sm" /></button><button class="btn btn-secondary" :disabled="!!saving" @click="pending = null">取消</button><button class="btn btn-primary" :disabled="!!saving" @click="saveSelection">{{ saving ? '正在保存...' : '保存基础价' }}</button></template>
    </BaseDialog>
    <BaseDialog :show="companyDialog" title="上游公司" width="wide" @close="companyDialog = false">
      <form class="company-form" @submit.prevent="saveCompany"><label>公司名称<input v-model="companyEdit.name" required maxlength="100" class="input" aria-label="公司名称" /></label><label>备注<input v-model="companyEdit.notes" class="input" aria-label="公司备注" /></label><button class="btn btn-primary" :disabled="companySaving">{{ companyEdit.id ? '保存修改' : '新建公司' }}</button><button v-if="companyEdit.id" type="button" class="icon-button" title="取消编辑" aria-label="取消编辑" @click="companyEdit = { id: 0, name: '', notes: '' }"><Icon name="x" size="sm" /></button></form>
      <p v-if="error" class="mt-3 text-sm text-red-600" role="alert">{{ error }}</p>
      <div v-for="company in companies" :key="company.id" class="company-list-row"><span>{{ company.name }}<small>{{ company.notes }}</small></span><div class="flex gap-2"><button class="icon-button" title="编辑公司" aria-label="编辑公司" @click="companyEdit = { ...company }"><Icon name="edit" size="sm" /></button><button class="icon-button" title="删除公司" aria-label="删除公司" @click="removeCompany(company)"><Icon name="trash" size="sm" /></button></div></div>
    </BaseDialog>
    <BaseDialog :show="!!historyModel" :title="`${historyModel} 基础价历史`" width="wide" @close="historyModel = ''">
      <p v-if="historyLoading" class="text-sm text-gray-500">正在读取...</p><p v-if="error" class="text-sm text-red-600" role="alert">{{ error }}</p>
      <details v-for="base in history" :key="base.version" class="history-entry"><summary><strong>v{{ base.version }}</strong><span>{{ new Date(base.updated_at).toLocaleString() }}</span><span>{{ sourceName(base) }}</span></summary><p v-if="base.price.conversion" class="policy-line">原报价 {{ base.price.conversion.source_currency }} · 生效汇率 1 USD = {{ base.price.conversion.usd_to_cny_rate }} CNY</p><div v-for="item in base.price.items" :key="itemKey(item)" class="history-price"><span>{{ itemLabel(item) }}</span><strong :title="item.amount">{{ formatPriceAmount(item.amount) }} {{ unitLabel(item.unit) }}</strong></div><div v-for="item in base.price.conversion?.source_items || []" :key="'source-' + itemKey(item)" class="history-price"><span>上游原价 · {{ itemLabel(item) }}</span><span>{{ item.amount }} {{ unitLabel(item.unit) }}</span></div><p v-if="base.price.policy.long_context_threshold" class="policy-line">{{ policyLabel(base.price) }}</p></details><p v-if="!historyLoading && !history.length" class="text-sm text-gray-500">暂无修改记录</p>
    </BaseDialog>
  </AppLayout>
</template>

<script setup lang="ts">
import { computed, onMounted, onUnmounted, ref, watch } from 'vue'
import AppLayout from '@/components/layout/AppLayout.vue'
import BaseDialog from '@/components/common/BaseDialog.vue'
import Icon from '@/components/icons/Icon.vue'
import { modelPricingAPI, candidateUSD, convertPriceAmount, formatPriceAmount, priceKindLabels, type PriceCatalog, type PriceAccount, type ModelPrice, type BasePrice, type PriceItem, type Company } from '@/api/admin/modelPricing'
import { extractApiErrorMessage } from '@/utils/apiError'

const catalog = ref<PriceCatalog>({ accounts: [], bases: [] }), companies = ref<Company[]>([])
const loading = ref(true), saving = ref(''), error = ref(''), success = ref(''), search = ref(''), companyFilter = ref(''), platformFilter = ref(''), modeFilter = ref(''), statusFilter = ref(''), activeModel = ref(''), page = ref(1)
const statuses = [{ value: '', label: '全部模型' }, { value: 'missing', label: '未定价' }, { value: 'priced', label: '已设置' }, { value: 'changed', label: '来源已变化' }]
const selected = ref<Record<string, number>>({}), hiddenAccounts = ref<number[]>([])
const companyDialog = ref(false), companySaving = ref(false), companyEdit = ref<Company>({ id: 0, name: '', notes: '' })
const historyModel = ref(''), history = ref<BasePrice[]>([]), historyLoading = ref(false)
const effectiveDialog = ref(false), accountFilterDialog = ref(false), filtersOpen = ref(false), quoteAccount = ref<PriceAccount | null>(null)
const quotePrice = computed(() => quoteAccount.value && findPrice(quoteAccount.value, activeModel.value))
type Row = { model: string; base?: BasePrice }
const pending = ref<{ model: string; price: ModelPrice; account: PriceAccount; base?: BasePrice } | null>(null)
const usdUnit = (unit: string) => unit.replace(/^CNY\//, 'USD/')
const itemKey = (item: PriceItem) => `${item.kind}:${item.tier}:${usdUnit(item.unit)}`
function cnyReference(amount: string) { return formatPriceAmount(convertPriceAmount(amount, 'USD', 'CNY', catalog.value.usd_to_cny_rate)) }
function itemLabel(item: PriceItem) {
  const tier = ({ priority: '优先', '5m': '5 分钟', '1h': '1 小时' } as Record<string, string>)[item.tier] || item.tier
  return `${priceKindLabels[item.kind] || item.kind}${tier ? ` · ${tier}` : ''}`
}
function unitLabel(unit: string) { return ({ 'USD/MTok': '美元 / 百万 Token', 'USD/image': '美元 / 张', 'USD/second': '美元 / 秒', 'USD/request': '美元 / 次', 'CNY/MTok': '人民币 / 百万 Token', 'CNY/image': '人民币 / 张', 'CNY/second': '人民币 / 秒', 'CNY/request': '人民币 / 次' } as Record<string, string>)[unit] || unit }
const confirmationItems = computed(() => [...new Map([...(pending.value?.price.items || []), ...(pending.value?.base?.price.items || [])].map(item => [itemKey(item), item])).values()])
function newAmount(price: ModelPrice, item: PriceItem) { const updated = price.items.find(i => itemKey(i) === itemKey(item)); return updated ? candidateUSD(updated, price, catalog.value.usd_to_cny_rate) : '移除' }
function currentAmount(base: BasePrice | undefined, item: PriceItem) { return base?.price.items.find(i => itemKey(i) === itemKey(item))?.amount || '未配置' }
const platforms = computed(() => [...new Set(catalog.value.accounts.map(a => a.platform))])
function findPrice(account: PriceAccount, model: string) { return account.prices.find(p => p.model === model) }
function sourceChanged(base: BasePrice) { const source = catalog.value.accounts.flatMap(a => a.prices).find(p => p.id === base.source_price_id); return !source || source.revision !== base.source_revision || (source.currency === 'CNY' && base.price.conversion?.usd_to_cny_rate !== catalog.value.usd_to_cny_rate) }
function sourceName(base: BasePrice) {
  const account = catalog.value.accounts.find(a => a.prices.some(p => p.id === base.source_price_id))
  return account ? `${account.company_name || '未归属'} / ${account.name}${account.status !== 'active' ? ' · 已停用' : ''}` : base.price.source_account_name ? `${base.price.source_company_name || '未归属'} / ${base.price.source_account_name} · 来源已移除` : `来源账号已移除 · 价格 #${base.source_price_id}`
}
const filteredAccounts = computed(() => catalog.value.accounts.filter(a => (!companyFilter.value || String(a.company_id || 0) === companyFilter.value) && (!platformFilter.value || a.platform === platformFilter.value)))
const rows = computed<Row[]>(() => {
  const models = new Set(Array.isArray(catalog.value.models) ? catalog.value.models : catalog.value.bases.map(b => b.model))
  if (!Array.isArray(catalog.value.models)) for (const account of catalog.value.accounts) { account.models.forEach(m => models.add(m)); account.prices.forEach(p => models.add(p.model)) }
  return [...models].sort().map(model => ({ model, base: catalog.value.bases.find(b => b.model === model) })).filter(row => {
    if (!row.model.toLowerCase().includes(search.value.toLowerCase())) return false
    if ((companyFilter.value || platformFilter.value) && !filteredAccounts.value.some(a => a.models.includes(row.model) || findPrice(a, row.model))) return false
    if (modeFilter.value && !catalog.value.accounts.some(a => findPrice(a, row.model)?.mode === modeFilter.value)) return false
    return !statusFilter.value || (statusFilter.value === 'missing' ? !row.base : statusFilter.value === 'changed' ? !!row.base && sourceChanged(row.base) : !!row.base)
  })
})
const totalRows = computed(() => catalog.value.total ?? rows.value.length)
const pageCount = computed(() => Math.max(1, Math.ceil(totalRows.value / 20)))
const pagedRows = computed(() => catalog.value.page ? rows.value : rows.value.slice((page.value - 1) * 20, page.value * 20))
const activeRow = computed(() => pagedRows.value.find(row => row.model === activeModel.value))
const supportedAccounts = computed(() => filteredAccounts.value.filter(a => activeRow.value && (a.models.includes(activeRow.value.model) || findPrice(a, activeRow.value.model))))
const comparisonGroups = computed(() => {
  const groups = new Map<number, { id: number; name: string; accounts: PriceAccount[] }>()
  for (const account of supportedAccounts.value) {
    if (hiddenAccounts.value.includes(account.id) || !findPrice(account, activeModel.value)) continue
    const id = account.company_id || 0
    if (!groups.has(id)) groups.set(id, { id, name: account.company_name, accounts: [] })
    groups.get(id)!.accounts.push(account)
  }
  return [...groups.values()]
})
const candidateAccounts = computed(() => comparisonGroups.value.flatMap(g => g.accounts))
const unpricedAccounts = computed(() => supportedAccounts.value.filter(a => !findPrice(a, activeModel.value)))
const comparisonItems = computed(() => [...new Map(candidateAccounts.value.flatMap(a => findPrice(a, activeModel.value)!.items).map(item => [itemKey(item), item])).values()])
const selectedAccount = computed(() => supportedAccounts.value.find(a => findPrice(a, activeModel.value)?.id === selected.value[activeModel.value]))
const canApply = computed(() => {
  const row = activeRow.value, price = selectedAccount.value && findPrice(selectedAccount.value, activeModel.value)
  return !!price && (!row?.base || row.base.source_price_id !== price.id || row.base.source_revision !== price.revision || (price.currency === 'CNY' && row.base.price.conversion?.usd_to_cny_rate !== catalog.value.usd_to_cny_rate))
})
function accountItem(account: PriceAccount, item: PriceItem) { return findPrice(account, activeModel.value)?.items.find(i => itemKey(i) === itemKey(item)) }
function comparisonAmount(account: PriceAccount, item: PriceItem) { const price = findPrice(account, activeModel.value), value = accountItem(account, item); return value && price ? candidateUSD(value, price, catalog.value.usd_to_cny_rate) : '未配置' }
function isEffectiveSource(account: PriceAccount) { return activeRow.value?.base?.source_price_id === findPrice(account, activeModel.value)?.id }
function rowMode(row: Row) { return modeLabel(row.base?.price.mode || catalog.value.accounts.map(a => findPrice(a, row.model)?.mode).find(Boolean) || '') }
function rowStatus(row: Row) { return !row.base ? '未定价' : sourceChanged(row.base) ? '来源已变化' : '已设置' }
function statusClass(row: Row) { return !row.base ? 'missing' : sourceChanged(row.base) ? 'changed' : 'priced' }
function modeLabel(mode: string) { return ({ token: 'Token', image: '图片按张', video: '视频按秒', per_request: '按次 / 按条' } as Record<string, string>)[mode] || '未配置' }
function policyLabel(price: ModelPrice) { return price.policy.long_context_threshold ? `超过 ${price.policy.long_context_threshold} Token · 输入 ×${price.policy.long_context_input_multiplier} · 输出 ×${price.policy.long_context_output_multiplier}` : '' }
function toggleAccount(id: number) {
  hiddenAccounts.value = hiddenAccounts.value.includes(id) ? hiddenAccounts.value.filter(v => v !== id) : [...hiddenAccounts.value, id]
}
function chooseModel(model: string) { activeModel.value = model; success.value = ''; effectiveDialog.value = false; quoteAccount.value = null; accountFilterDialog.value = false }
let loadSequence = 0, historySequence = 0
let refreshTimer: ReturnType<typeof setTimeout> | undefined
function scheduleLoad() { clearTimeout(refreshTimer); refreshTimer = setTimeout(() => { void load() }, 200) }
watch([search, companyFilter, platformFilter, modeFilter, statusFilter], () => { page.value = 1; success.value = ''; scheduleLoad() })
watch(page, scheduleLoad)
onUnmounted(() => { clearTimeout(refreshTimer); loadSequence++; historySequence++ })
function refreshComparison() { pending.value = null; void load() }
async function load() {
  const sequence = ++loadSequence
  loading.value = true; error.value = ''
  try {
    const [data, list] = await Promise.all([modelPricingAPI.catalog({ page: page.value, page_size: 20, search: search.value, company_id: companyFilter.value, platform: platformFilter.value, mode: modeFilter.value, status: statusFilter.value }), modelPricingAPI.companies()])
    if (sequence !== loadSequence) return
    catalog.value = data; companies.value = list
    selected.value = Object.fromEntries(data.bases.map(b => [b.model, b.source_price_id]))
    page.value = data.page || Math.min(page.value, pageCount.value)
    if (!pagedRows.value.some(row => row.model === activeModel.value)) activeModel.value = pagedRows.value[0]?.model || ''
  } catch (e) { if (sequence === loadSequence) error.value = extractApiErrorMessage(e, '读取模型价格失败') }
  finally { if (sequence === loadSequence) loading.value = false }
}
function preview(row: Row) {
  const account = selectedAccount.value, price = account && findPrice(account, row.model)
  if (account && price && !loading.value) { error.value = ''; success.value = ''; pending.value = { model: row.model, account, price, base: row.base } }
}
async function saveSelection() {
  const value = pending.value
  if (!value || saving.value) return
  saving.value = value.model; error.value = ''
  try { await modelPricingAPI.select(value.model, value.price, value.base?.version || 0, catalog.value.usd_to_cny_rate); pending.value = null; await load(); success.value = `${value.model} 的基础价格已生效` }
  catch (e) { error.value = extractApiErrorMessage(e, '保存模型基础价格失败') }
  finally { saving.value = '' }
}
async function saveCompany() {
  companySaving.value = true; error.value = ''
  try { await modelPricingAPI.saveCompany(companyEdit.value); companyEdit.value = { id: 0, name: '', notes: '' }; await load() }
  catch (e) { error.value = extractApiErrorMessage(e, '保存公司失败') }
  finally { companySaving.value = false }
}
async function removeCompany(company: Company) {
  if (!window.confirm(`删除上游公司“${company.name}”？`)) return
  try { await modelPricingAPI.deleteCompany(company.id); await load() }
  catch (e) { error.value = extractApiErrorMessage(e, '删除公司失败') }
}
async function showHistory(model: string) {
  const sequence = ++historySequence
  historyModel.value = model; historyLoading.value = true; history.value = []; error.value = ''
  try { const list = await modelPricingAPI.history(model); if (sequence === historySequence) history.value = list }
  catch (e) { if (sequence === historySequence) error.value = extractApiErrorMessage(e, '读取历史失败') }
  finally { if (sequence === historySequence) historyLoading.value = false }
}
onMounted(load)
</script>

<style scoped>
.pricing-layout { height: 100dvh; min-height: 0; overflow: hidden; }
.pricing-layout :deep(main) { display: flex; flex-direction: column; height: calc(100dvh - 65px); min-height: 0; overflow: hidden; padding-block: 20px; }
.pricing-page { display: flex; flex: 1; flex-direction: column; min-width: 0; min-height: 0; overflow: hidden; color: #1f2937; letter-spacing: 0; }
.page-heading { display: flex; flex-shrink: 0; align-items: center; justify-content: space-between; gap: 12px; padding-bottom: 16px; }
.page-heading h1 { font-size: 20px; font-weight: 600; }
.count-label { color: #6b7280; font-size: 12px; white-space: nowrap; }
.heading-actions, .detail-actions, .comparison-actions { display: flex; align-items: center; gap: 8px; flex-shrink: 0; }
.heading-actions .btn, .selection-footer .btn { display: inline-flex; align-items: center; justify-content: center; gap: 7px; min-height: 34px; font-size: 12px; border-radius: 6px; }
.icon-button { width: 34px; height: 34px; display: flex; align-items: center; justify-content: center; flex-shrink: 0; color: #6b7280; border: 1px solid #e5e7eb; border-radius: 6px; background: white; }
.icon-button:hover { background: #f3f4f6; color: #111827; }
.icon-button:disabled { opacity: .4; cursor: default; }
.pricing-toolbar { display: grid; flex-shrink: 0; grid-template-columns: minmax(180px, 1.2fr) minmax(0, 3fr); gap: 12px; padding-bottom: 10px; }
.pricing-filters { display: grid; grid-template-columns: repeat(3, minmax(0, 1fr)); gap: 10px; }
.pricing-page :deep(.input), .company-form .input { min-width: 0; border-radius: 6px; font-size: 12px; }
.search-field { position: relative; min-width: 0; }
.search-field > svg { position: absolute; left: 12px; top: 50%; transform: translateY(-50%); color: #9ca3af; }
.search-field .input { padding-left: 36px; }
.mobile-filter-toggle, .mobile-model-picker { display: none; }
.status-tabs { display: flex; flex-shrink: 0; gap: 24px; border-bottom: 1px solid #e5e7eb; }
.status-tabs button { position: relative; padding: 10px 0; color: #6b7280; font-size: 12px; white-space: nowrap; }
.status-tabs button.active { color: #047857; font-weight: 600; }
.status-tabs button.active::after { position: absolute; bottom: -1px; left: 0; right: 0; height: 2px; background: #059669; content: ''; }
.feedback { display: flex; flex-shrink: 0; flex-wrap: wrap; align-items: center; gap: 8px; max-height: 72px; overflow-y: auto; font-size: 12px; padding: 8px 0; overflow-wrap: anywhere; }
.error-feedback { color: #dc2626; }
.success-feedback { color: #047857; }
.pricing-workspace { display: grid; flex: 1; grid-template-columns: 220px minmax(0, 1fr); min-height: 0; min-width: 0; overflow: hidden; }
.model-list { min-width: 0; min-height: 0; overflow: hidden; border-right: 1px solid #e5e7eb; display: flex; flex-direction: column; }
.list-heading { display: flex; flex-shrink: 0; justify-content: space-between; padding: 16px; color: #6b7280; font-size: 11px; }
.model-list-content { flex: 1; min-height: 0; overflow-y: auto; overscroll-behavior: contain; scrollbar-gutter: stable; }
.model-option { display: flex; flex-direction: column; gap: 8px; width: 100%; text-align: left; padding: 13px 16px; border-left: 3px solid transparent; }
.model-option:hover { background: #f3f4f6; }
.model-option.active { background: #ecfdf5; border-left-color: #059669; }
.model-name { font-size: 12px; font-weight: 500; overflow-wrap: anywhere; }
.model-option-meta { display: flex; align-items: center; justify-content: space-between; flex-wrap: wrap; gap: 6px; font-size: 10px; color: #6b7280; }
.price-status { display: inline-flex; align-items: center; gap: 5px; font-size: 11px; font-weight: 400; }
.price-status::before { content: ''; width: 5px; height: 5px; border-radius: 50%; background: currentColor; flex-shrink: 0; }
.price-status.priced { color: #059669; }
.price-status.missing { color: #b45309; }
.price-status.changed { color: #c2410c; }
.list-empty { padding: 32px 16px; text-align: center; color: #6b7280; font-size: 12px; }
.list-pagination { display: flex; flex-shrink: 0; align-items: center; justify-content: space-between; padding: 8px 16px; border-top: 1px solid #e5e7eb; font-size: 11px; color: #6b7280; }
.list-pagination .icon-button { width: 28px; height: 28px; border: 0; }
.model-detail { display: flex; flex-direction: column; min-width: 0; min-height: 0; overflow: hidden; padding: 16px 0 0 24px; }
.detail-heading { display: flex; flex-shrink: 0; align-items: flex-start; justify-content: space-between; gap: 12px; }
.detail-heading h2 { margin-bottom: 5px; font-size: 16px; font-weight: 600; overflow-wrap: anywhere; max-height: 48px; overflow-y: auto; }
.history-button { display: inline-flex; align-items: center; justify-content: center; gap: 6px; height: 34px; font-size: 12px; color: #4b5563; white-space: nowrap; }
.effective-summary { display: flex; flex-shrink: 0; align-items: center; gap: 8px; min-width: 0; margin-top: 10px; font-size: 11px; }
.summary-label, .version { color: #6b7280; white-space: nowrap; }
.effective-summary strong { min-width: 0; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; font-weight: 500; color: #4b5563; }
.source-warning { display: flex; flex-shrink: 0; align-items: flex-start; gap: 6px; padding: 7px 10px; margin-top: 10px; max-height: 56px; overflow-y: auto; color: #b45309; background: #fffbeb; font-size: 11px; }
.source-warning svg { flex-shrink: 0; }
.policy-line { margin-top: 12px; color: #6b7280; font-size: 12px; overflow-wrap: anywhere; }
.comparison-heading { display: flex; flex-shrink: 0; align-items: center; justify-content: space-between; gap: 8px; padding: 12px 0 8px; }
.comparison-heading h3 { font-size: 13px; font-weight: 600; }
.comparison-heading h3 > span { margin-left: 10px; color: #6b7280; font-size: 11px; font-weight: 400; }
.missing-quotes { font-size: 11px; color: #b45309; }
.comparison-scroll { flex: 1; min-height: 0; min-width: 0; overflow: auto; overscroll-behavior: contain; scrollbar-gutter: stable; border: 1px solid #e5e7eb; border-radius: 6px; background: white; }
.comparison-scroll:focus-visible { outline: 2px solid #059669; outline-offset: -2px; }
.comparison-table { width: 100%; border-collapse: separate; border-spacing: 0; text-align: left; font-size: 12px; }
.comparison-table th, .comparison-table td { padding: 14px 16px; border-bottom: 1px solid #e5e7eb; vertical-align: middle; background: white; }
.comparison-table tr:last-child > * { border-bottom: 0; }
.comparison-table th { font-weight: 500; }
.comparison-table thead th { position: sticky; top: 0; z-index: 3; background: #f9fafb; vertical-align: top; font-size: 11px; }
.price-column { min-width: 146px; }
.price-column > span { display: block; margin-top: 5px; color: #6b7280; font-size: 10px; font-weight: 400; }
.account-column { position: sticky; left: 0; z-index: 2; width: 230px; min-width: 230px; max-width: 230px; border-right: 1px solid #e5e7eb; }
.comparison-table thead .account-column { z-index: 4; }
.account-identity { display: flex; align-items: flex-start; justify-content: space-between; gap: 6px; }
.account-name { min-width: 0; font-size: 12px; font-weight: 600; overflow-wrap: anywhere; }
.account-company { display: block; margin: 6px 0 0 23px; color: #6b7280; font-size: 11px; overflow-wrap: anywhere; }
.account-meta { display: flex; flex-wrap: wrap; align-items: center; gap: 6px; margin: 5px 0 0 23px; color: #6b7280; font-size: 10px; font-weight: 400; }
.quote-detail-button { display: grid; place-items: center; width: 24px; height: 24px; border-radius: 4px; color: #6b7280; flex-shrink: 0; margin-top: -3px; }
.quote-detail-button:hover { background: #e5e7eb; color: #111827; }
.comparison-table .chosen > * { background: #f0fdf4; }
.comparison-table tbody tr:hover > * { background: #f9fafb; }
.comparison-table tbody tr.chosen:hover > * { background: #ecfdf5; }
.quote-amount { font-size: 14px; font-weight: 600; font-variant-numeric: tabular-nums; overflow-wrap: anywhere; }
.original-price { color: #6b7280; }
.select-source { display: flex; align-items: flex-start; gap: 9px; min-width: 0; cursor: pointer; }
.select-source input { flex-shrink: 0; margin-top: 2px; }
.select-source input, .account-options input { accent-color: #059669; }
.effective-source { color: #047857; font-size: 10px; }
.no-quotes, .empty-workspace { display: flex; flex: 1; min-height: 0; overflow-y: auto; flex-direction: column; justify-content: center; align-items: center; gap: 14px; padding: 20px 12px; color: #6b7280; font-size: 13px; }
.pricing-link { display: inline-flex; align-items: center; gap: 6px; font-size: 12px; color: #047857; }
.selection-footer { display: flex; flex-shrink: 0; align-items: center; justify-content: space-between; gap: 16px; min-height: 72px; padding: 12px 0 0; }
.selection-summary { min-width: 0; display: flex; flex-direction: column; gap: 4px; font-size: 11px; color: #6b7280; }
.selection-summary strong { font-size: 12px; font-weight: 500; color: #374151; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
.selection-footer .btn { flex-shrink: 0; }
.confirmation-source { display: flex; flex-direction: column; gap: 8px; margin-bottom: 20px; overflow-wrap: anywhere; font-size: 14px; }
.confirmation-source strong { color: #6b7280; font-size: 12px; font-weight: 400; }
.confirmation-source small { font-size: 12px; color: #6b7280; }
.confirmation-scroll { overflow: auto; }
.confirmation-table { width: 100%; font-size: 12px; text-align: left; }
.confirmation-table th { padding: 10px 12px; color: #6b7280; background: #f9fafb; font-weight: 500; }
.confirmation-table td { padding: 12px; border-bottom: 1px solid #e5e7eb; overflow-wrap: anywhere; min-width: 90px; }
.confirmation-table small { display: block; margin-top: 5px; color: #6b7280; font-size: 10px; }
.new-price { color: #047857; font-weight: 600; }
.account-options label { display: flex; align-items: flex-start; gap: 10px; padding: 12px 0; border-bottom: 1px solid #e5e7eb; font-size: 12px; overflow-wrap: anywhere; }
.account-options input { flex-shrink: 0; margin-top: 2px; }
.account-options small { display: block; margin-top: 5px; color: #b45309; }
.company-form { display: flex; flex-wrap: wrap; align-items: end; gap: 12px; margin-bottom: 24px; }
.company-form label { min-width: 0; flex: 1 1 160px; font-size: 12px; }
.company-form .input { margin-top: 8px; }
.company-list-row { display: flex; align-items: center; justify-content: space-between; gap: 12px; border-top: 1px solid #e5e7eb; padding: 14px 0; font-size: 13px; }
.company-list-row > span { min-width: 0; overflow-wrap: anywhere; }
.company-list-row small { display: block; color: #6b7280; margin-top: 5px; font-size: 11px; }
.history-entry { padding: 16px 0; border-bottom: 1px solid #e5e7eb; }
.history-entry summary { cursor: pointer; font-size: 12px; overflow-wrap: anywhere; }
.history-entry summary > span { margin-left: 12px; color: #6b7280; }
.history-price { display: flex; flex-wrap: wrap; justify-content: space-between; gap: 8px; padding: 10px 0; font-size: 12px; }
.history-price strong { font-weight: 500; overflow-wrap: anywhere; }
.pricing-page:where(.dark *) { color: #e5e7eb; }
.pricing-page:where(.dark *) :is(.status-tabs, .model-list, .list-pagination, .comparison-scroll, .comparison-table th, .comparison-table td) { border-color: #374151; }
.pricing-page:where(.dark *) :is(.comparison-scroll, .comparison-table th, .comparison-table td) { background: #111827; }
.pricing-page:where(.dark *) :is(.comparison-table thead th, .icon-button, .model-option:hover, .comparison-table tbody tr:hover > *) { background: #1f2937; }
.pricing-page:where(.dark *) :is(.model-option.active, .comparison-table .chosen > *, .comparison-table tbody tr.chosen:hover > *) { background: #052e25; }
.pricing-page:where(.dark *) :is(.selection-summary strong, .effective-summary strong, .history-button) { color: #d1d5db; }
.pricing-page:where(.dark *) :is(.pricing-link, .success-feedback, .effective-source, .status-tabs button.active, .price-status.priced) { color: #6ee7b7; }
.pricing-page:where(.dark *) :is(.summary-label, .version, .account-company, .account-meta, .count-label, .list-heading, .price-column > span, .list-pagination, .icon-button) { color: #9ca3af; }
.pricing-page:where(.dark *) .source-warning { background: #422006; color: #fcd34d; }
.confirmation-table:where(.dark *) th { background: #1f2937; color: #d1d5db; }
.confirmation-table:where(.dark *) td, .account-options:where(.dark *) label { border-color: #374151; }
.confirmation-table:where(.dark *) .new-price { color: #6ee7b7; }
@media (max-width: 1100px) {
  .pricing-workspace { grid-template-columns: 190px minmax(0, 1fr); }
  .model-detail { padding-left: 16px; }
  .account-column { width: 200px; min-width: 200px; max-width: 200px; }
}
@media (max-width: 767px) {
  .pricing-layout :deep(main) { padding: 12px; }
  .page-heading { gap: 8px; padding-bottom: 10px; }
  .page-heading h1 { font-size: 17px; }
  .page-heading .count-label { display: none; }
  .heading-actions { gap: 6px; }
  .heading-actions .btn { width: 32px; min-height: 32px; padding: 0; }
  .heading-actions .btn span { display: none; }
  .heading-actions .icon-button { width: 32px; height: 32px; }
  .pricing-toolbar { grid-template-columns: minmax(0, 1fr) 36px; gap: 8px; padding-bottom: 4px; }
  .mobile-filter-toggle { display: flex; height: 38px; width: 36px; }
  .pricing-filters { display: none; grid-column: 1 / -1; grid-template-columns: repeat(3, minmax(0, 1fr)); gap: 6px; }
  .filters-open .pricing-filters { display: grid; padding-bottom: 4px; }
  .pricing-filters .input { padding-inline: 8px; font-size: 11px; }
  .status-tabs { gap: 0; justify-content: space-between; }
  .status-tabs button { padding-block: 9px; font-size: 11px; }
  .pricing-workspace { display: flex; flex-direction: column; }
  .model-list { display: grid; grid-template-columns: minmax(0, 1fr) auto; align-items: center; gap: 8px; flex-shrink: 0; padding-block: 8px; border-right: 0; border-bottom: 1px solid #e5e7eb; }
  .list-heading, .model-list-content { display: none; }
  .mobile-model-picker { display: block; width: 100%; height: 34px; padding-block: 5px; padding-left: 8px; font-size: 12px; text-overflow: ellipsis; }
  .list-pagination { padding: 0; border: 0; gap: 4px; font-size: 10px; }
  .list-pagination .icon-button { width: 24px; }
  .model-detail { flex: 1; padding: 10px 0 0; }
  .detail-heading { gap: 8px; }
  .detail-heading h2 { font-size: 14px; max-height: 40px; }
  .detail-actions { gap: 6px; }
  .detail-actions .icon-button { width: 28px; height: 28px; }
  .history-button { height: 28px; font-size: 11px; gap: 4px; }
  .effective-summary { margin-top: 6px; font-size: 10px; gap: 6px; }
  .source-warning { margin-top: 6px; padding: 5px 8px; font-size: 10px; }
  .comparison-heading { padding-top: 8px; }
  .comparison-heading h3 { font-size: 12px; }
  .comparison-heading h3 > span { margin-left: 6px; font-size: 10px; }
  .comparison-actions { gap: 6px; }
  .comparison-actions .icon-button { width: 28px; height: 28px; }
  .missing-quotes { font-size: 10px; }
  .comparison-table th, .comparison-table td { padding: 12px 10px; }
  .account-column { width: 150px; min-width: 150px; max-width: 150px; }
  .price-column { min-width: 128px; }
  .account-name { font-size: 11px; }
  .account-company { font-size: 10px; }
  .account-meta { font-size: 9px; }
  .quote-amount { font-size: 13px; }
  .selection-footer { min-height: 64px; gap: 12px; padding-top: 8px; }
  .selection-footer .btn { padding-inline: 12px; }
  .selection-summary { font-size: 10px; }
  .selection-summary strong { font-size: 11px; }
}
</style>
