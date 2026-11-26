<template>
  <section class="account-pricing">
    <header class="pricing-context">
      <div class="account-context"><strong>{{ accountName || '未命名账号' }}</strong><span>{{ companyName || '未归属' }}</span></div>
      <a href="/admin/model-pricing" target="_blank" rel="noopener" class="pricing-link">模型基础价格<Icon name="arrowRight" size="sm" /></a>
    </header>
    <p v-if="error" class="pricing-error" role="alert">{{ error }} <button type="button" class="underline" @click="load">重试</button></p>
    <p v-if="!modelValue.loaded" class="pricing-empty">正在读取账号报价...</p>
    <div v-else-if="!modelValue.prices.length" class="pricing-empty"><Icon name="calculator" size="lg" /><span>暂无模型报价</span></div>
    <div v-else class="quote-workspace">
      <aside class="quote-sidebar">
        <div class="model-controls">
          <div :class="['quote-progress', { 'has-missing': missingWhitelistModels.length }]">
            <span v-if="whitelistModels.length">白名单模型 {{ whitelistModels.length }} 个 · 已填写 {{ configuredWhitelistCount }} 个</span>
            <span v-else>模型 {{ modelValue.prices.length }} 个</span>
            <button v-if="missingWhitelistModels.length" type="button" class="quote-icon small" title="补齐白名单报价" aria-label="补齐白名单报价" @click="addWhitelistPrices"><Icon name="plus" size="sm" /></button>
          </div>
          <div class="model-search">
            <Icon name="search" size="sm" />
            <input v-model="search" type="search" aria-label="搜索报价模型" placeholder="搜索模型" />
            <button v-if="search" type="button" title="清空搜索" aria-label="清空模型搜索" @click="search = ''"><Icon name="x" size="sm" /></button>
          </div>
          <div class="model-filters" role="tablist" aria-label="报价状态筛选">
            <button v-for="filter in filters" :key="filter.value" type="button" role="tab" :aria-selected="statusFilter === filter.value" :class="{ active: statusFilter === filter.value }" @click="statusFilter = filter.value">{{ filter.label }}</button>
          </div>
          <select v-if="visiblePrices.length" class="input mobile-model-picker" aria-label="选择报价模型" :value="activeIndex" @change="selectPrice(Number(($event.target as HTMLSelectElement).value))">
            <option v-if="!visiblePrices.some(row => row.index === activeIndex)" :value="activeIndex">{{ activePrice?.model }}</option>
            <option v-for="{ price, index } in visiblePrices" :key="price.model" :value="index">{{ price.model }} · {{ priceStatus(price) }}</option>
          </select>
          <p v-else class="mobile-no-results" role="status">没有匹配的模型</p>
        </div>
        <nav class="quote-models" aria-label="账号报价模型">
          <button v-for="{ price, index } in visiblePrices" :key="price.model" type="button" :class="['quote-model', { active: activeIndex === index }]" :aria-current="activeIndex === index ? 'true' : undefined" @click="selectPrice(index)">
            <span class="model-name">{{ price.model || '未命名模型' }}</span>
            <span class="model-meta"><span>{{ modeLabel(price.mode) }}</span><span :class="['quote-state', { dirty: isDirty(price), complete: isComplete(price) && !isDirty(price) }]">{{ priceStatus(price) }}</span></span>
          </button>
          <p v-if="!visiblePrices.length" class="model-list-empty">没有匹配的模型</p>
        </nav>
      </aside>
      <div v-if="activePrice" class="quote-editor">
        <div class="editor-title"><div><h4>{{ activePrice.model || '新增模型' }}</h4><span :class="['quote-state', { dirty: isDirty(activePrice), complete: isComplete(activePrice) && !isDirty(activePrice) }]">{{ priceStatus(activePrice) }}</span></div><button type="button" class="quote-icon" title="移除模型报价" aria-label="移除模型报价" @click="removePrice(activeIndex)"><Icon name="trash" size="sm" /></button></div>
        <div ref="editorBody" class="quote-editor-body">
          <div class="quote-currency-row">
            <label>报价币种<select :value="activePrice.currency || 'USD'" class="input" aria-label="报价币种" @change="setCurrency($event)"><option value="USD">美元 USD</option><option value="CNY">人民币 CNY</option></select></label>
            <span v-if="exchangeRate">统一汇率：1 USD = {{ exchangeRate }} CNY</span>
            <button v-else type="button" class="pricing-link" @click="loadExchangeRate">重试读取汇率</button>
          </div>
          <p v-if="exchangeError" class="exchange-error" role="alert">{{ exchangeError }}</p>
          <fieldset class="billing-fieldset">
            <legend>计费方式</legend>
            <div class="billing-modes">
              <button v-for="mode in billingModes" :key="mode.value" type="button" :class="{ active: activePrice.mode === mode.value }" :aria-pressed="activePrice.mode === mode.value" @click="changeMode(activePrice, mode.value)">{{ mode.label }}</button>
            </div>
          </fieldset>
          <div class="adjustments">
            <label>采购折扣系数<input v-model="activePrice.discount" required inputmode="decimal" pattern="(0([.][0-9]{1,12})?|1([.]0{1,12})?)" class="input" aria-label="采购折扣系数" title="五折填 0.5，不打折填 1" /></label>
            <label>公司加价倍数<input v-model="activePrice.markup" required inputmode="decimal" pattern="[0-9]+([.][0-9]{1,12})?" class="input" aria-label="公司加价倍数" title="加价 20% 填 1.2，不加价填 1" /></label>
          </div>
          <div class="price-section-heading"><h5>{{ activePrice.mode === 'token' ? '常用价格' : '规格价格' }}</h5><span>{{ unitLabel(activePrice.mode, activePrice.currency) }}</span></div>
          <ModelPriceItemFields :price="activePrice" :fields="standardFields(activePrice)" :exchange-rate="exchangeRate" @update:items="activePrice.items = $event" />
          <details v-if="activePrice.mode === 'token'" :key="activePrice.model + '-advanced'" class="advanced-pricing" :open="hasAdvanced(activePrice)">
            <summary><span>高级价格</span><Icon name="chevronDown" size="sm" /></summary>
            <section v-for="group in optionalGroups" :key="group.key" class="advanced-group">
              <h5>{{ group.label }}</h5>
              <ModelPriceItemFields :price="activePrice" :fields="group.fields" :exchange-rate="exchangeRate" @update:items="activePrice.items = $event" />
            </section>
            <section class="advanced-group">
              <h5>长上下文策略</h5>
              <div class="context-fields">
                <label>阈值 Token<input v-model.number="activePrice.policy.long_context_threshold" type="number" min="0" class="input" aria-label="长上下文阈值 Token" /></label>
                <label>输入倍率<input v-model.number="activePrice.policy.long_context_input_multiplier" type="number" min="1" step="any" class="input" /></label>
                <label>输出倍率<input v-model.number="activePrice.policy.long_context_output_multiplier" type="number" min="1" step="any" class="input" /></label>
              </div>
            </section>
          </details>
          <div v-else-if="activePrice.mode === 'per_request'" class="request-tier-row">
            <input v-model="requestTier" class="input" aria-label="新增按次计费规格" placeholder="新增规格，例如 1K、720p" maxlength="32" @keydown.enter.prevent="addRequestTier(activePrice)" />
            <button type="button" class="quote-icon" title="添加计费规格" aria-label="添加计费规格" :disabled="!requestTier.trim()" @click="addRequestTier(activePrice)"><Icon name="plus" size="sm" /></button>
          </div>
        </div>
      </div>
    </div>
  </section>
</template>

<script setup lang="ts">
import { computed, onMounted, ref, watch } from 'vue'
import Icon from '@/components/icons/Icon.vue'
import ModelPriceItemFields, { type PriceField } from './ModelPriceItemFields.vue'
import { modelPricingAPI, newModelPrice, changePriceCurrency, pricingUnit, type PriceCurrency, type AccountPricingDraft, type ModelPrice, type PriceMode } from '@/api/admin/modelPricing'
import { extractApiErrorMessage } from '@/utils/apiError'

const props = defineProps<{ modelValue: AccountPricingDraft; accountId?: number; models?: string[]; restrictedModels?: string[]; mappings?: { from: string; to: string }[]; accountName?: string; companyName?: string }>()
const emit = defineEmits<{ 'update:modelValue': [AccountPricingDraft]; 'dirty-count': [number] }>()
const error = ref(''), activeIndex = ref(0), requestTier = ref(''), search = ref('')
const statusFilter = ref<'all' | 'missing' | 'dirty'>('all')
const editorBody = ref<HTMLElement | null>(null)
const initialPrices = ref<Record<string, string>>({}), savedModels = ref<string[]>([])
const filters = [{ value: 'all', label: '全部' }, { value: 'missing', label: '待填写' }, { value: 'dirty', label: '未保存' }] as const
const exchangeRate = ref(''), exchangeError = ref('')
const activePrice = computed(() => props.modelValue.prices[activeIndex.value])
const whitelistModels = computed(() => [...new Set(props.restrictedModels || [])].filter(m => m && !/[\s*?]/.test(m)))
const missingWhitelistModels = computed(() => whitelistModels.value.filter(model => !props.modelValue.prices.some(price => price.model === model)))
const configuredWhitelistCount = computed(() => props.modelValue.prices.filter(price => whitelistModels.value.includes(price.model) && isComplete(price)).length)
const configuredModels = computed(() => [...new Set([...whitelistModels.value, ...(props.models || [])])].filter(m => m && !/[\s*?]/.test(m)))
const billingModes: { value: PriceMode; label: string }[] = [{ value: 'token', label: 'Token' }, { value: 'image', label: '图片 / 张' }, { value: 'video', label: '视频 / 秒' }, { value: 'per_request', label: '按次 / 条' }]
function modeLabel(mode: PriceMode) { return billingModes.find(m => m.value === mode)?.label }
function unitLabel(mode: PriceMode, currency: PriceCurrency = 'USD') { return `${currency === 'CNY' ? '人民币' : '美元'} / ${({ token: '百万 Token', image: '张', video: '秒', per_request: '次' })[mode]}` }
const field = (kind: string, label: string, tier = '', required = false): PriceField => ({ kind, label, tier, required })
const optionalGroups = [
  { key: 'cache', label: '缓存写入时长', fields: [field('cache_write', '缓存写入 · 5 分钟', '5m'), field('cache_write', '缓存写入 · 1 小时', '1h')] },
  { key: 'image', label: '图片 Token 价格', fields: [field('image_input', '图片输入 Token'), field('image_output', '图片输出 Token')] },
  { key: 'priority', label: '优先服务价格', fields: [field('input', '优先输入', 'priority'), field('output', '优先输出', 'priority'), field('cache_read', '优先缓存读取', 'priority'), field('cache_write', '优先缓存写入', 'priority')] }
]
function configuredCount(price: ModelPrice, fields: PriceField[]) { return fields.filter(f => price.items.some(i => i.kind === f.kind && i.tier === f.tier && i.amount !== '')).length }
function hasGroup(price: ModelPrice, fields: PriceField[]) { return configuredCount(price, fields) > 0 }
function standardFields(price: ModelPrice): PriceField[] {
  if (price.mode === 'token') return [field('input', '输入', '', true), field('output', '输出', '', true), field('cache_read', '缓存读取'), field('cache_write', '缓存写入')]
  if (price.mode === 'image') return ['1K', '2K', '4K'].map(t => field('image', t, t))
  if (price.mode === 'video') return ['480p', '720p', '1080p'].map(t => field('video', t, t))
  return [...new Set(['', ...price.items.map(i => i.tier)])].map(t => field('request', t || '标准', t))
}
function isComplete(price: ModelPrice) {
  const required = standardFields(price).filter(f => f.required)
  return price.items.some(i => i.amount !== '') && required.every(f => price.items.some(i => i.kind === f.kind && i.tier === f.tier && i.amount !== ''))
}

function priceSignature(price: ModelPrice) {
  return JSON.stringify({
    mode: price.mode, currency: price.currency || 'USD', upstream: price.upstream_model, discount: price.discount, markup: price.markup,
    items: price.items.filter(item => item.amount !== '').map(item => ({ ...item })).sort((a, b) => `${a.kind}:${a.tier}`.localeCompare(`${b.kind}:${b.tier}`)),
    policy: Object.entries(price.policy).sort(([a], [b]) => a.localeCompare(b))
  })
}
function isDirty(price: ModelPrice) { return priceSignature(price) !== (initialPrices.value[price.model] ?? priceSignature(makePrice(price.model))) }
const dirtyModels = computed(() => new Set([
  ...props.modelValue.prices.filter(isDirty).map(price => price.model),
  ...savedModels.value.filter(model => !props.modelValue.prices.some(price => price.model === model))
]))
const visiblePrices = computed(() => props.modelValue.prices.map((price, index) => ({ price, index })).filter(({ price }) =>
  price.model.toLowerCase().includes(search.value.trim().toLowerCase()) &&
  (statusFilter.value === 'all' || (statusFilter.value === 'missing' ? !isComplete(price) : isDirty(price)))))
function selectPrice(index: number) { activeIndex.value = index }
function priceStatus(price: ModelPrice) { return isDirty(price) ? '未保存' : isComplete(price) ? '已配置' : '待填写' }
function hasAdvanced(price: ModelPrice) { return optionalGroups.some(group => hasGroup(price, group.fields)) || !!price.policy.long_context_threshold }
watch([search, statusFilter], () => {
  if (visiblePrices.value.length && !visiblePrices.value.some(row => row.index === activeIndex.value)) selectPrice(visiblePrices.value[0].index)
})
watch(activeIndex, () => { requestTier.value = ''; if (editorBody.value) editorBody.value.scrollTop = 0 })
watch(() => dirtyModels.value.size, count => emit('dirty-count', count), { immediate: true })
let loadSequence = 0
async function loadExchangeRate() {
  exchangeError.value = ''
  try { exchangeRate.value = (await modelPricingAPI.exchangeRate()).usd_to_cny_rate }
  catch (e) { exchangeRate.value = ''; exchangeError.value = extractApiErrorMessage(e, '无法读取统一汇率，人民币报价暂不能预览换算') }
}
function setCurrency(event: Event) {
  const select = event.target as HTMLSelectElement
  const currency = select.value as PriceCurrency
  const price = activePrice.value
  if (!price || (price.currency || 'USD') === currency) return
  if (price.items.some(item => item.amount !== '') && !window.confirm('切换币种会保留填写的数值并按新币种解释，请确认原价币种正确。')) {
    select.value = price.currency || 'USD'
    return
  }
  changePriceCurrency(price, currency)
}
async function load() {
  const seq = ++loadSequence
  error.value = ''
  try {
    const prices = props.accountId ? await modelPricingAPI.accountPrices(props.accountId) : { prices: props.modelValue.prices, version: '' }
    if (seq !== loadSequence) return
    const existing = new Set(prices.prices.map(price => price.model))
    const additions = configuredModels.value.filter(model => !existing.has(model)).map(makePrice)
    const loadedPrices = [...prices.prices, ...additions]
    initialPrices.value = Object.fromEntries(loadedPrices.map(price => [price.model, priceSignature(price)]))
    savedModels.value = prices.prices.filter(price => price.id > 0).map(price => price.model)
    emit('update:modelValue', { ...props.modelValue, prices: loadedPrices, version: prices.version, loaded: true })
  } catch (e) { if (seq === loadSequence) error.value = extractApiErrorMessage(e, '读取账号价格失败，暂时无法保存账号价格') }
}
function makePrice(model: string) {
  const price = newModelPrice(model)
  price.upstream_model = props.mappings?.find(mapping => mapping.from === model)?.to || model
  return price
}
function addWhitelistPrices() {
  if (!props.modelValue.loaded) return
  emit('update:modelValue', { ...props.modelValue, prices: [...props.modelValue.prices, ...missingWhitelistModels.value.map(makePrice)] })
}
function removePrice(index: number) {
  const price = props.modelValue.prices[index]
  if (price && (price.id > 0 || price.items.some(i => i.amount !== '')) && !window.confirm(`移除模型“${price.model}”的账号报价？`)) return
  activeIndex.value = Math.max(0, index - 1)
  emit('update:modelValue', { ...props.modelValue, prices: props.modelValue.prices.filter((_, i) => i !== index) })
}
function changeMode(price: ModelPrice, mode: PriceMode) {
  if (price.mode === mode) return
  if (price.items.some(i => i.amount !== '') && !window.confirm('切换计费方式会清空当前模型的报价，是否继续？')) return
  price.mode = mode
  price.items = []
  price.policy = {}
  requestTier.value = ''
  if (mode === 'token') price.items = newModelPrice().items
  changePriceCurrency(price, price.currency || 'USD')
}
function addRequestTier(price: ModelPrice) {
  const tier = requestTier.value.trim()
  if (tier && !price.items.some(i => i.tier === tier)) price.items.push({ kind: 'request', tier, unit: pricingUnit(price.mode, price.currency), amount: '' })
  requestTier.value = ''
}
watch(() => props.accountId, () => { activeIndex.value = 0; void load() })
watch(() => props.modelValue.prices.length, length => { if (activeIndex.value >= length) activeIndex.value = Math.max(0, length - 1) })
watch(() => [...configuredModels.value], (models, previous) => {
  if (!props.modelValue.loaded) return
  // Remove only untouched placeholders from the previous model selection.
  const prices = props.modelValue.prices.filter(price => models.includes(price.model) || !previous.includes(price.model)
    || price.id > 0 || price.items.some(item => item.amount !== '') || price.mode !== 'token'
    || price.discount !== '1' || price.markup !== '1' || Object.keys(price.policy).length > 0)
  const additions = models.filter(model => !previous.includes(model) && !prices.some(price => price.model === model)).map(makePrice)
  if (prices.length !== props.modelValue.prices.length || additions.length) {
    const selected = activePrice.value
    const updated = [...prices, ...additions]
    activeIndex.value = Math.max(0, updated.indexOf(selected))
    emit('update:modelValue', { ...props.modelValue, prices: updated })
  }
})
watch(() => props.modelValue.loaded, ready => { if (!ready) void load() })
onMounted(load)
onMounted(loadExchangeRate)
</script>

<style scoped>
.account-pricing { flex: 1; display: flex; flex-direction: column; min-height: 0; min-width: 0; overflow: hidden; }
.pricing-context { display: flex; align-items: center; justify-content: space-between; gap: 12px; flex-shrink: 0; padding: 14px 24px; border-bottom: 1px solid #e5e7eb; }
.account-context { min-width: 0; display: flex; align-items: center; gap: 12px; font-size: 12px; }
.account-context strong { font-weight: 500; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
.account-context > span { color: #6b7280; overflow-wrap: anywhere; }
.pricing-link { display: inline-flex; align-items: center; gap: 6px; flex-shrink: 0; font-size: 12px; color: #047857; }
.pricing-error { flex-shrink: 0; padding: 10px 24px; font-size: 12px; color: #dc2626; }
.pricing-empty { display: flex; flex: 1; flex-direction: column; align-items: center; justify-content: center; gap: 12px; padding: 24px; color: #9ca3af; font-size: 13px; }
.quote-workspace { flex: 1; min-height: 0; min-width: 0; display: grid; grid-template-columns: 260px minmax(0, 1fr); overflow: hidden; }
.quote-sidebar { display: flex; flex-direction: column; min-height: 0; min-width: 0; border-right: 1px solid #e5e7eb; background: #fafbfc; }
.model-controls { padding: 14px 16px 0; flex-shrink: 0; }
.quote-progress { min-height: 24px; display: flex; align-items: center; justify-content: space-between; gap: 6px; margin-bottom: 10px; font-size: 11px; color: #6b7280; }
.model-search { display: flex; align-items: center; gap: 8px; height: 36px; padding: 0 10px; border: 1px solid #d1d5db; border-radius: 6px; color: #9ca3af; background: white; }
.model-search:focus-within { border-color: #059669; box-shadow: 0 0 0 2px #10b9811a; }
.model-search input { min-width: 0; flex: 1; border: 0; outline: 0; padding: 0; background: transparent; color: #111827; font-size: 12px; }
.model-search input::-webkit-search-cancel-button { display: none; }
.model-search > button { display: grid; place-items: center; width: 20px; height: 24px; }
.model-filters { display: flex; gap: 18px; margin-top: 10px; border-bottom: 1px solid #e5e7eb; }
.model-filters button { position: relative; padding: 9px 0 11px; font-size: 12px; color: #6b7280; }
.model-filters button.active { color: #047857; font-weight: 600; }
.model-filters button.active::after { content: ''; position: absolute; bottom: -1px; left: 0; right: 0; height: 2px; background: #059669; }
.quote-models { flex: 1; min-height: 0; overflow-y: auto; overscroll-behavior: contain; padding: 8px; scrollbar-gutter: stable; }
.quote-model { display: flex; flex-direction: column; gap: 8px; width: 100%; text-align: left; border-left: 2px solid transparent; padding: 12px 10px; font-size: 12px; border-radius: 0 4px 4px 0; }
.quote-model:hover { background: #f0f2f4; }
.quote-model.active { border-left-color: #059669; background: #e8f5ee; color: #065f46; }
.model-name { overflow-wrap: anywhere; }
.model-meta { display: flex; flex-wrap: wrap; align-items: center; justify-content: space-between; gap: 6px; font-size: 10px; color: #6b7280; }
.quote-state { color: #6b7280; font-size: 11px; }
.quote-state.dirty { color: #b45309; }
.quote-state.complete { color: #047857; }
.model-list-empty { padding: 32px 8px; color: #9ca3af; text-align: center; font-size: 12px; }
.mobile-model-picker, .mobile-no-results { display: none; }
.quote-editor { min-height: 0; min-width: 0; display: flex; flex-direction: column; overflow: hidden; }
.editor-title { display: flex; align-items: center; justify-content: space-between; gap: 12px; flex-shrink: 0; min-height: 74px; padding: 14px 24px; border-bottom: 1px solid #e5e7eb; }
.editor-title > div { min-width: 0; }
.editor-title h4 { font-size: 15px; font-weight: 600; overflow-wrap: anywhere; margin-bottom: 4px; }
.quote-editor-body { flex: 1; min-height: 0; overflow-y: auto; overscroll-behavior: contain; padding: 20px 24px 32px; scrollbar-gutter: stable; }
.quote-icon { width: 32px; height: 32px; display: inline-flex; align-items: center; justify-content: center; flex-shrink: 0; border: 1px solid #d1d5db; border-radius: 6px; color: #6b7280; }
.quote-icon.small { width: 24px; height: 24px; }
.quote-icon:hover { color: #111827; background: #f3f4f6; }
.quote-icon:disabled { opacity: .4; }
.account-pricing :deep(.input) { border-radius: 6px; font-size: 13px; }
.billing-fieldset legend { margin-bottom: 8px; font-size: 12px; color: #6b7280; }
.quote-currency-row { display: flex; align-items: flex-end; flex-wrap: wrap; gap: 12px 24px; margin-bottom: 20px; font-size: 12px; color: #6b7280; }
.quote-currency-row label { width: 160px; }
.quote-currency-row .input { margin-top: 8px; }
.quote-currency-row > span { padding-bottom: 12px; }
.exchange-error { margin-bottom: 12px; color: #b45309; font-size: 12px; }
.billing-modes { display: grid; grid-template-columns: repeat(4, minmax(0, 1fr)); padding: 3px; border: 1px solid #e5e7eb; border-radius: 6px; background: #f9fafb; }
.billing-modes button { min-width: 0; text-align: center; font-size: 12px; padding: 8px 2px; border-radius: 4px; }
.billing-modes button.active { background: white; color: #047857; box-shadow: 0 1px 3px #00000012; font-weight: 600; }
.billing-modes button:focus-visible, .quote-model:focus-visible { outline: 2px solid #10b981; outline-offset: -2px; }
.adjustments { display: grid; grid-template-columns: repeat(2, minmax(0, 1fr)); gap: 16px; padding: 20px 0 24px; }
.adjustments label, .context-fields label { font-size: 12px; color: #6b7280; }
.adjustments .input, .context-fields .input { margin-top: 8px; }
.price-section-heading { display: flex; align-items: center; justify-content: space-between; gap: 8px; }
.price-section-heading h5, .advanced-group h5 { font-size: 13px; font-weight: 600; }
.price-section-heading > span { color: #6b7280; font-size: 11px; }
.advanced-pricing { margin-top: 16px; border-top: 1px solid #e5e7eb; }
.advanced-pricing > summary { display: flex; align-items: center; justify-content: space-between; gap: 8px; padding: 16px 0; font-size: 12px; cursor: pointer; list-style: none; }
.advanced-pricing > summary::-webkit-details-marker { display: none; }
.advanced-pricing[open] > summary :deep(svg) { transform: rotate(180deg); }
.advanced-group { padding: 12px 0; border-top: 1px solid #e5e7eb; }
.context-fields { display: grid; grid-template-columns: repeat(3, minmax(0, 1fr)); gap: 12px; padding-top: 14px; }
.request-tier-row { display: flex; gap: 8px; padding-top: 12px; }
.request-tier-row .input { min-width: 0; flex: 1; }
:global(.dark) .pricing-context, :global(.dark) .quote-sidebar, :global(.dark) .model-filters, :global(.dark) .editor-title, :global(.dark) .advanced-pricing, :global(.dark) .advanced-group, :global(.dark) .billing-modes { border-color: #374151; }
:global(.dark) .quote-sidebar, :global(.dark) .billing-modes { background: #151e2b; }
:global(.dark) .model-search, :global(.dark) .billing-modes button.active { background: #1f2937; border-color: #4b5563; }
:global(.dark) .model-search input { color: #e5e7eb; }
:global(.dark) .quote-model:hover { background: #1f2937; }
:global(.dark) .quote-model.active { background: #064e3b; color: #a7f3d0; }
:global(.dark) .pricing-link, :global(.dark) .quote-state.complete, :global(.dark) .model-filters button.active { color: #6ee7b7; }
:global(.dark) .quote-state.dirty { color: #fbbf24; }
@media (max-width: 767px) {
  .pricing-context { padding: 10px 16px; }
  .account-context { flex-direction: column; align-items: flex-start; gap: 3px; }
  .account-context strong { max-width: 180px; }
  .pricing-link { font-size: 11px; }
  .quote-workspace { display: flex; flex-direction: column; }
  .quote-sidebar { flex-shrink: 0; border-right: 0; border-bottom: 1px solid #e5e7eb; }
  .model-controls { padding: 10px 16px; }
  .quote-progress { display: none; }
  .quote-progress.has-missing { display: flex; min-height: 24px; margin-bottom: 6px; }
  .model-filters { margin-top: 3px; margin-bottom: 10px; }
  .model-filters button { padding-block: 7px; }
  .quote-models { display: none; }
  .mobile-model-picker { display: block; height: 36px; padding-block: 6px; font-size: 12px; }
  .mobile-no-results { display: flex; align-items: center; height: 36px; color: #9ca3af; font-size: 12px; }
  .quote-editor { flex: 1; }
  .editor-title { padding: 10px 16px; min-height: 64px; }
  .editor-title h4 { font-size: 13px; }
  .quote-editor-body { padding: 16px 16px 24px; }
  .adjustments { gap: 12px; padding-block: 16px; }
  .context-fields { grid-template-columns: minmax(0, 1fr); }
}
</style>
