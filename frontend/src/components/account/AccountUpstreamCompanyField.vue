<template>
  <div class="upstream-company-field">
    <label class="input-label" :for="fieldId">所属上游公司</label>
    <div class="company-controls">
      <select :id="fieldId" :value="modelValue" class="input" aria-label="所属上游公司" :disabled="loading" @change="$emit('update:modelValue', Number(($event.target as HTMLSelectElement).value))">
        <option :value="0">未归属</option>
        <option v-for="company in companies" :key="company.id" :value="company.id">{{ company.name }}</option>
      </select>
      <button type="button" class="btn btn-secondary company-toggle" :title="showCompany ? '取消新建公司' : '新建上游公司'" :aria-label="showCompany ? '取消新建公司' : '新建上游公司'" @click="showCompany = !showCompany"><Icon :name="showCompany ? 'x' : 'plus'" size="sm" /></button>
    </div>
    <div v-if="showCompany" class="company-create">
      <input v-model="name" class="input" aria-label="上游公司名称" placeholder="公司名称" maxlength="100" @keydown.enter.prevent="save" />
      <button type="button" class="btn btn-secondary" :disabled="!name.trim() || saving" @click="save">保存公司</button>
    </div>
    <p v-if="error" class="mt-2 text-sm text-red-600" role="alert">{{ error }} <button type="button" class="underline" @click="load">重试</button></p>
  </div>
</template>

<script setup lang="ts">
import { computed, onMounted, ref, useId, watch } from 'vue'
import Icon from '@/components/icons/Icon.vue'
import { modelPricingAPI, type Company } from '@/api/admin/modelPricing'
import { extractApiErrorMessage } from '@/utils/apiError'

const props = defineProps<{ modelValue: number }>()
const emit = defineEmits<{ 'update:modelValue': [number]; 'company-name': [string] }>()
const fieldId = useId()
const companies = ref<Company[]>([])
const loading = ref(false), saving = ref(false), showCompany = ref(false), name = ref(''), error = ref('')
const selectedName = computed(() => companies.value.find(company => company.id === props.modelValue)?.name || '未归属')
watch(selectedName, value => emit('company-name', value), { immediate: true })
async function load() {
  loading.value = true; error.value = ''
  try { companies.value = await modelPricingAPI.companies() }
  catch (e) { error.value = extractApiErrorMessage(e, '读取上游公司失败') }
  finally { loading.value = false }
}
async function save() {
  if (!name.value.trim() || saving.value) return
  saving.value = true; error.value = ''
  try {
    const company = await modelPricingAPI.saveCompany({ id: 0, name: name.value.trim(), notes: '' })
    companies.value.push(company)
    emit('update:modelValue', company.id)
    name.value = ''; showCompany.value = false
  } catch (e) { error.value = extractApiErrorMessage(e, '保存上游公司失败') }
  finally { saving.value = false }
}
onMounted(load)
</script>

<style scoped>
.company-controls, .company-create { display: flex; align-items: center; gap: 8px; }
.company-controls .input, .company-create .input { flex: 1; min-width: 0; }
.company-toggle { width: 42px; height: 42px; padding: 0; display: grid; place-items: center; flex-shrink: 0; }
.company-create { margin-top: 8px; }
.company-create .btn { flex-shrink: 0; }
</style>
