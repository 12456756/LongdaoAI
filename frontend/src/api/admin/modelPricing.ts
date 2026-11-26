import { apiClient } from '../client'

export type PriceMode = 'token' | 'image' | 'video' | 'per_request'
export type PriceCurrency = 'USD' | 'CNY'
export interface PriceItem { kind: string; tier: string; unit: string; amount: string }
export interface ModelPrice {
	source_account_name?: string; source_company_name?: string
  id: number; account_id: number; model: string; upstream_model: string; mode: PriceMode
  discount: string; markup: string; items: PriceItem[]; revision: number
  currency?: PriceCurrency
  conversion?: { source_currency: PriceCurrency; usd_to_cny_rate: string; source_items: PriceItem[] }
  policy: { long_context_threshold?: number; long_context_input_multiplier?: number; long_context_output_multiplier?: number }
}
export interface Company { id: number; name: string; notes: string }
export interface BasePrice { model: string; source_price_id: number; source_revision: number; version: number; price: ModelPrice; updated_by: number; updated_at: string }
export interface PriceAccount { id: number; name: string; company_id: number | null; company_name: string; platform: string; status: string; models: string[]; prices: ModelPrice[] }
export interface PriceCatalog { accounts: PriceAccount[]; bases: BasePrice[]; usd_to_cny_rate?: string; models?: string[]; total?: number; page?: number; page_size?: number }
export interface AccountPricingDraft { companyId: number; prices: ModelPrice[]; loaded: boolean; version?: string }

export const modelPricingAPI = {
  exchangeRate: async () => (await apiClient.get<{ usd_to_cny_rate: string }>('/admin/model-pricing/exchange-rate')).data,
  companies: async () => (await apiClient.get<Company[]>('/admin/upstream-companies')).data,
  saveCompany: async (company: Company) => (await apiClient.post<Company>('/admin/upstream-companies', company)).data,
  deleteCompany: async (id: number) => (await apiClient.delete(`/admin/upstream-companies/${id}`)).data,
  accountPrices: async (id: number) => (await apiClient.get<{ prices: ModelPrice[]; version: string; usd_to_cny_rate?: string }>(`/admin/accounts/${id}/model-prices`)).data,
  catalog: async (params?: { page: number; page_size: number; search: string; company_id: string; platform: string; mode: string; status: string }) => (await apiClient.get<PriceCatalog>('/admin/model-pricing', { params })).data,
  select: async (model: string, price: ModelPrice, version: number, exchangeRate?: string) => (await apiClient.put<BasePrice>('/admin/model-pricing/base-price', {
    model, source_price_id: price.id, source_revision: price.revision, expected_version: version, expected_exchange_rate: exchangeRate
  })).data,
  history: async (model: string) => (await apiClient.get<BasePrice[]>('/admin/model-pricing/history', { params: { model } })).data
}

export const priceKindLabels: Record<string, string> = {
  input: '输入', output: '输出', cache_read: '缓存读取', cache_write: '缓存写入', image_input: '图片输入 Token', image_output: '图片输出 Token', image: '图片', video: '视频', request: '按次'
}

export function newModelPrice(model = ''): ModelPrice {
  return { id: 0, account_id: 0, model, upstream_model: model, mode: 'token', currency: 'USD', discount: '1', markup: '1', revision: 0,
    items: [{ kind: 'input', tier: '', unit: 'USD/MTok', amount: '' }, { kind: 'output', tier: '', unit: 'USD/MTok', amount: '' }], policy: {} }
}

export function configuredPrices(prices: ModelPrice[]) {
  return prices.filter(p => p.id > 0 || p.items.some(item => item.amount !== ''))
}

export function priceLabel(item: PriceItem) { return `${priceKindLabels[item.kind] || item.kind}${item.tier ? ` / ${item.tier}` : ''}` }

export function candidateAmount(item: PriceItem, price: ModelPrice) {
  const values = [item.amount, price.discount, price.markup]
  if (!values.every(v => /^(0|[1-9][0-9]{0,11})(\.[0-9]{1,12})?$/.test(v))) return '-'
  let scale = 0
  let product = BigInt(1)
  for (const value of values) {
    const [whole, fraction = ''] = value.split('.')
    scale += fraction.length
    product *= BigInt(whole + fraction)
  }
  const digits = product.toString().padStart(scale + 1, '0')
  if (!scale) return digits
  return `${digits.slice(0, -scale)}.${digits.slice(-scale)}`.replace(/\.?0+$/, '')
}

export function pricingUnit(mode: PriceMode, currency: PriceCurrency = 'USD') {
  return `${currency}/${({ token: 'MTok', image: 'image', video: 'second', per_request: 'request' } as const)[mode]}`
}

export function changePriceCurrency(price: ModelPrice, currency: PriceCurrency) {
  price.currency = currency
  price.items = price.items.map(item => ({ ...item, unit: pricingUnit(price.mode, currency) }))
}

const decimalPattern = /^(0|[1-9][0-9]*)(\.[0-9]+)?$/
export function convertPriceAmount(amount: string, from: PriceCurrency, to: PriceCurrency, exchangeRate?: string) {
  if (!decimalPattern.test(amount)) return '-'
  if (from === to) return amount
  if (!exchangeRate || !decimalPattern.test(exchangeRate)) return '-'
  const parse = (value: string) => { const [whole, fraction = ''] = value.split('.'); return { n: BigInt(whole + fraction), scale: fraction.length } }
  const a = parse(amount), r = parse(exchangeRate)
  if (r.n === 0n) return '-'
  let numerator: bigint, denominator: bigint
  if (from === 'CNY') { numerator = a.n * 10n ** BigInt(r.scale); denominator = r.n * 10n ** BigInt(a.scale) }
  else { numerator = a.n * r.n; denominator = 10n ** BigInt(a.scale + r.scale) }
  const precision = 36n
  const scaled = numerator * 10n ** precision
  const rounded = scaled / denominator + (scaled % denominator * 2n >= denominator ? 1n : 0n)
  const digits = rounded.toString().padStart(37, '0')
  return `${digits.slice(0, -36)}.${digits.slice(-36)}`.replace(/\.?0+$/, '')
}

export function candidateUSD(item: PriceItem, price: ModelPrice, exchangeRate?: string) {
  return convertPriceAmount(candidateAmount(item, price), price.currency || 'USD', 'USD', exchangeRate)
}

export function formatPriceAmount(amount: string, decimals = 12) {
  if (amount === '-') return amount
  const [whole, fraction] = amount.split('.')
  if (!fraction) return amount
  const precision = whole === '0' ? Math.max(decimals, fraction.search(/[1-9]/) + 3) : decimals
  return `${whole}.${fraction.slice(0, precision)}`.replace(/\.?0+$/, '')
}
