<template>
  <div class="account-form-tabs" role="tablist" aria-label="账号设置">
    <button type="button" role="tab" :aria-selected="modelValue === 'account'" :class="{ active: modelValue === 'account' }" @click="$emit('update:modelValue', 'account')">账号配置</button>
    <button type="button" role="tab" :aria-selected="modelValue === 'pricing'" :class="{ active: modelValue === 'pricing' }" @click="$emit('update:modelValue', 'pricing')">模型报价<span v-if="count">{{ count }}</span></button>
  </div>
</template>

<script setup lang="ts">
defineProps<{ modelValue: string; count: number }>()
defineEmits<{ 'update:modelValue': [string] }>()
</script>

<style scoped>
.account-form-tabs { display: flex; gap: 24px; border-bottom: 1px solid #e5e7eb; margin-bottom: 20px; }
.account-form-tabs button { position: relative; padding: 8px 0 14px; font-size: 13px; color: #6b7280; }
.account-form-tabs button.active { color: #047857; font-weight: 600; }
.account-form-tabs button.active::after { content: ''; position: absolute; left: 0; right: 0; bottom: -1px; height: 2px; background: #059669; }
.account-form-tabs span { margin-left: 7px; color: #9ca3af; font-size: 11px; font-weight: 400; }
:global(.dark) .account-form-tabs { border-color: #374151; }
:global(.dark) .account-form-tabs button.active { color: #6ee7b7; }
</style>
