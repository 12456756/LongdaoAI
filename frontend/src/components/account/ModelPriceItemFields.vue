<template>
  <div class="price-fields">
    <div class="field-head" aria-hidden="true"><span>价格项目</span><span>上游原价 · {{ price.currency || 'USD' }}</span><span>公司采购价 · {{ price.currency || 'USD' }}</span><span>候选基础价 · USD</span></div>
    <div v-for="field in fields" :key="`${field.kind}:${field.tier}`" class="field-row">
      <label :for="fieldId(field)" class="field-label">{{ field.label }}<span v-if="field.required" class="text-red-500"> *</span></label>
      <input :id="fieldId(field)" :value="item(field).amount" :required="field.required && (price.id > 0 || price.items.some(i => i.amount !== ''))" inputmode="decimal" pattern="[0-9]+([.][0-9]{1,12})?" class="input" :placeholder="field.required ? '填写原价' : '未配置'" :aria-label="`${price.model} ${field.kind}${field.tier ? ` ${field.tier}` : ''} 原价`" @input="update(field, ($event.target as HTMLInputElement).value)" />
      <span class="result procurement"><span class="mobile-label">采购价 {{ price.currency || 'USD' }}</span>{{ procurement(field) }}</span>
      <span class="result candidate"><span class="mobile-label">基础价 USD</span><span :title="candidateUSD(item(field), price, exchangeRate)">{{ formatPriceAmount(candidateUSD(item(field), price, exchangeRate)) }}</span><small v-if="candidateUSD(item(field), price, exchangeRate) !== '-' && exchangeRate">≈ ¥{{ formatPriceAmount(candidateCNY(field)) }}</small></span>
    </div>
  </div>
</template>

<script setup lang="ts">
import { useId } from 'vue'
import { candidateAmount, candidateUSD, convertPriceAmount, formatPriceAmount, pricingUnit, type ModelPrice, type PriceItem } from '@/api/admin/modelPricing'

export interface PriceField { kind: string; tier: string; label: string; required?: boolean }
const props = defineProps<{ price: ModelPrice; fields: PriceField[]; exchangeRate?: string }>()
const emit = defineEmits<{ 'update:items': [PriceItem[]] }>()
const id = useId()
function fieldId(field: PriceField) { return `${id}-${field.kind}-${field.tier || 'standard'}` }
function item(field: PriceField): PriceItem {
  return props.price.items.find(i => i.kind === field.kind && i.tier === field.tier) || {
    kind: field.kind, tier: field.tier, amount: '',
    unit: pricingUnit(props.price.mode, props.price.currency)
  }
}
function procurement(field: PriceField) { return candidateAmount(item(field), { ...props.price, markup: '1' }) }
function candidateCNY(field: PriceField) { return convertPriceAmount(candidateAmount(item(field), props.price), props.price.currency || 'USD', 'CNY', props.exchangeRate) }
function update(field: PriceField, amount: string) {
  const index = props.price.items.findIndex(i => i.kind === field.kind && i.tier === field.tier)
  if (!amount && !field.required) {
    if (index >= 0) emit('update:items', props.price.items.filter((_, i) => i !== index))
  } else if (index >= 0) emit('update:items', props.price.items.map((item, i) => i === index ? { ...item, amount } : item))
  else emit('update:items', [...props.price.items, { ...item(field), amount }])
}
</script>

<style scoped>
.field-head, .field-row { display: grid; grid-template-columns: minmax(100px, 1.2fr) minmax(90px, 1fr) minmax(75px, 1fr) minmax(85px, 1fr); align-items: center; gap: 12px; }
.field-head { padding: 10px 0; font-size: 11px; color: #6b7280; }
.field-row { min-height: 58px; border-top: 1px solid #e5e7eb; font-size: 12px; }
.field-row .input { width: 100%; min-width: 0; border-radius: 6px; padding: 8px 10px; font-size: 13px; }
.field-label { overflow-wrap: anywhere; }
.result { min-width: 0; overflow-wrap: anywhere; font-variant-numeric: tabular-nums; }
.procurement { color: #6b7280; }
.candidate { color: #047857; font-weight: 600; }
.candidate > small { display: block; font-size: 10px; color: #6b7280; font-weight: 400; }
.mobile-label { display: none; }
:global(.dark) .field-row { border-color: #374151; }
:global(.dark) .candidate { color: #6ee7b7; }
@media (max-width: 640px) {
  .field-head { display: none; }
  .field-row { grid-template-columns: minmax(0, 1fr) minmax(0, 1fr); padding: 12px 0; gap: 8px 12px; }
  .field-label { font-size: 13px; }
  .result { display: flex; flex-wrap: wrap; gap: 6px; font-size: 12px; }
  .mobile-label { display: inline; font-weight: 400; color: #6b7280; }
}
</style>
