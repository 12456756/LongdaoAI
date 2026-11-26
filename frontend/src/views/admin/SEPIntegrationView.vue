<template>
  <AppLayout>
    <div class="sep-page">
      <header class="sep-heading">
        <div>
          <h1>SEP 企业对接</h1>
          <span :class="['state', config.enabled ? 'on' : 'off']">{{
            config.enabled ? "已启用" : "未启用"
          }}</span>
        </div>
        <button class="btn btn-secondary" :disabled="loading" @click="load">
          <Icon name="refresh" size="sm" />刷新
        </button>
      </header>
      <nav class="sep-tabs" role="tablist" aria-label="SEP 管理视图">
        <button
          v-for="tab in tabs"
          :key="tab.key"
          role="tab"
          :aria-selected="active === tab.key"
          :class="{ selected: active === tab.key }"
          @click="active = tab.key"
        >
          {{ tab.label }}
        </button>
      </nav>
      <p v-if="error" class="feedback error" role="alert">
        {{ error
        }}<button @click="error = ''" aria-label="关闭错误">
          <Icon name="x" size="sm" />
        </button>
      </p>
      <p v-if="success" class="feedback success" role="status">{{ success }}</p>
      <div v-if="loading" class="py-12 text-center text-gray-500">
        加载中...
      </div>
      <form
        v-else-if="active === 'config'"
        class="sep-form"
        @submit.prevent="save"
      >
        <section class="config-section">
          <div class="section-label">
            <h2>集成身份</h2>
            <label class="enable-label"
              ><input v-model="config.enabled" type="checkbox" />启用 SEP
              对接</label
            >
          </div>
          <div class="field-grid">
            <label
              >集成编号<input
                v-model="config.integration_id"
                class="input"
                :readonly="integrationLocked"
                placeholder="sep"
            /></label>
            <div class="rate-readout">
              <span>平台统一汇率</span><strong>1 USD = {{ rate }} CNY</strong
              ><router-link to="/admin/settings?tab=payment"
                >汇率设置<Icon name="externalLink" size="sm"
              /></router-link>
            </div>
          </div>
        </section>
        <section class="config-section">
          <h2>开户默认分组</h2>
          <div class="group-grid">
            <label v-for="purpose in purposes" :key="purpose.kind"
              >{{ purpose.label }} Key<select
                v-model.number="config[purpose.field]"
                :aria-label="`${purpose.label} Key`"
                class="input"
              >
                <option :value="0">选择分组</option>
                <option
                  v-for="group in availableGroups(purpose.kind)"
                  :key="group.id"
                  :value="group.id"
                >
                  {{ group.name }} · {{ group.platform }} ·
                  {{ group.rate_multiplier }}x
                </option>
              </select></label
            >
          </div>
        </section>
        <section class="config-section">
          <h2>服务签名公钥</h2>
          <div class="field-grid">
            <label
              >签名公钥版本<input
                v-model="config.signing_key_id"
                class="input"
                placeholder="sep-signing-v1" /></label
            ><span class="algorithm">Ed25519</span>
          </div>
          <label class="pem-label"
            >公钥 PEM<textarea
              v-model="config.signing_public_key"
              class="input pem"
              rows="4"
              spellcheck="false"
              placeholder="-----BEGIN PUBLIC KEY-----"
            />
          </label>
        </section>
        <section class="config-section">
          <h2>Key 交付加密公钥</h2>
          <div class="field-grid">
            <label
              >加密公钥版本<input
                v-model="config.encryption_key_id"
                class="input"
                placeholder="sep-encryption-v1" /></label
            ><span class="algorithm">RSA-OAEP-256 / A256GCM · RSA ≥ 3072</span>
          </div>
          <label class="pem-label"
            >公钥 PEM<textarea
              v-model="config.encryption_public_key"
              class="input pem"
              rows="6"
              spellcheck="false"
              placeholder="-----BEGIN PUBLIC KEY-----"
            />
          </label>
        </section>
        <section class="config-section">
          <h2>余额同步</h2>
          <label>SEP 接收地址<input v-model="config.balance_webhook_url" class="input" aria-label="SEP 余额通知地址" placeholder="https://sep.example.com/compute/balance-events" /></label>
          <label class="enable-label"><input v-model="config.direct_billing_enabled" type="checkbox" />允许算力平台独立消费</label>
          <div class="flex items-center gap-3"><button type="button" class="btn btn-secondary" @click="loadSigningKey"><Icon name="key" size="sm" />查看通知签名公钥</button><span>{{ eventKey?.key_id }}</span></div>
          <textarea v-if="eventKey?.public_key" :value="eventKey.public_key" readonly class="input pem" rows="4" aria-label="算力通知签名公钥" />
          <p v-if="eventKey?.error" role="alert" class="text-sm text-red-600 break-words">{{ eventKey.error }}</p>
        </section>
        <footer class="save-bar">
          <button class="btn btn-primary" type="submit" :disabled="saving">
            {{ saving ? "保存中..." : "保存配置" }}
          </button>
        </footer>
      </form>
      <section v-else class="enterprise-section">
        <div class="enterprise-toolbar">
          <input
            v-model="search"
            type="search"
            class="input"
            placeholder="搜索企业名称、编号或邮箱"
            aria-label="搜索企业"
          /><span>{{ filtered.length }} 家企业</span>
        </div>
        <p v-if="!filtered.length" class="empty-state">
          {{ search ? "没有匹配的企业" : "暂无已开户企业" }}
        </p>
        <div v-else class="table-scroll">
          <table>
            <thead>
              <tr>
                <th>企业</th>
                <th>企业编号</th>
                <th>管理员邮箱</th>
                <th>状态</th>
                <th>平台账号</th>
                <th />
              </tr>
            </thead>
            <tbody>
              <tr
                v-for="enterprise in filtered"
                :key="enterprise.enterprise_id"
              >
                <td>{{ enterprise.company_name }}</td>
                <td class="mono">{{ enterprise.enterprise_id }}</td>
                <td>{{ enterprise.admin_email }}</td>
                <td>
                  <span
                    :class="[
                      'state',
                      enterprise.status === 'ready' ? 'on' : 'off',
                    ]"
                    >{{
                      enterprise.status === "ready" ? "正常" : "已停用"
                    }}</span
                  >
                </td>
                <td>{{ enterprise.compute_account_id }}</td>
                <td>
                  <button
                    class="btn btn-secondary btn-sm"
                    @click="openEnterprise(enterprise.enterprise_id)"
                  >
                    查看
                  </button>
                </td>
              </tr>
            </tbody>
          </table>
        </div>
      </section>
      <BaseDialog
        :show="!!detail"
        title="企业资金与凭证"
        width="wide"
        @close="detail = null"
      >
        <div v-if="detail" class="enterprise-detail">
          <p v-if="error" role="alert" class="text-sm text-red-600 break-words">{{ error }}</p>
          <div>
            <h2>{{ detail.enterprise.company_name }}</h2>
            <p class="mono text-sm text-gray-500">
              {{ detail.enterprise.enterprise_id }}
            </p>
          </div>
          <dl class="balance-strip">
            <div>
              <dt>可用人民币</dt>
              <dd>¥{{ detail.balance.available_paid_cny }}</dd>
            </div>
            <div>
              <dt>冻结人民币</dt>
              <dd>¥{{ detail.balance.frozen_paid_cny }}</dd>
            </div>
            <div>
              <dt>账务序号</dt>
              <dd>{{ detail.balance.sequence }}</dd>
            </div>
          </dl>
          <div class="detail-status">
            <label
              >企业状态<select v-model="detailStatus" class="input">
                <option value="ready">正常</option>
                <option value="disabled">停用</option>
              </select></label
            >
          </div>
          <div class="key-rows">
            <div
              v-for="credential in detail.enterprise.credentials"
              :key="credential.credential_id"
              class="key-row"
            >
              <span
                >{{ purposeName(credential.type) }} Key<small
                  >{{ credential.masked_key }} · v{{
                    credential.credential_version
                  }}</small
                ></span
              ><select
                v-model.number="detailGroups[credential.type]"
                class="input"
                :aria-label="`${purposeName(credential.type)} Key 分组`"
              >
                <option
                  v-for="group in availableGroups(credential.type)"
                  :key="group.id"
                  :value="group.id"
                >
                  {{ group.name }}
                </option>
              </select>
            </div>
          </div>
          <div class="detail-actions">
            <button
              class="btn btn-secondary"
              :disabled="saving"
              @click="loadLedger(false)"
            >
              <Icon name="refresh" size="sm" />刷新流水</button
            ><button
              class="btn btn-primary"
              :disabled="saving"
              @click="saveEnterprise"
            >
              保存企业设置
            </button>
          </div>
          <h2>执行费用</h2>
          <div class="table-scroll">
            <table>
              <thead>
                <tr>
                  <th>执行编号</th>
                  <th>模型</th>
                  <th>费用状态</th>
                  <th>人民币总费用</th>
                  <th>结算</th>
                </tr>
              </thead>
              <tbody>
                <tr v-for="bill in detail.bills" :key="bill.execution_id">
                  <td class="mono">{{ bill.execution_id }}</td>
                  <td>{{ bill.model_id }}</td>
                  <td>{{ feeStatus(bill.fee_status) }}</td>
                  <td>{{ bill.gross_cny ?? "-" }}</td>
                  <td>{{ bill.settlement_id ? "已结算" : "待结算" }}<small v-if="bill.billing_error" class="block text-red-600 break-words">{{ bill.billing_error }}</small></td>
                </tr>
                <tr v-if="!detail.bills?.length">
                  <td colspan="5" class="empty-state">暂无执行记录</td>
                </tr>
              </tbody>
            </table>
          </div>
          <h2>账务流水</h2>
          <div class="table-scroll">
            <table>
              <thead>
                <tr>
                  <th>序号</th>
                  <th>类型</th>
                  <th>付费金额</th>
                  <th>免费记录</th>
                  <th>业务编号</th>
                </tr>
              </thead>
              <tbody>
                <tr v-for="entry in ledger?.items" :key="entry.ledger_entry_id">
                  <td>{{ entry.sequence }}</td>
                  <td>{{ ledgerType(entry.type) }}</td>
                  <td>{{ entry.amount_cny }}</td>
                  <td>{{ entry.free_cny }}</td>
                  <td class="mono">
                    {{ entry.transfer_id || entry.execution_id }}
                  </td>
                </tr>
                <tr v-if="!ledger?.items.length">
                  <td colspan="5" class="empty-state">暂无账务流水</td>
                </tr>
              </tbody>
            </table>
          </div>
          <button
            v-if="ledger?.has_more"
            class="btn btn-secondary"
            @click="loadLedger(true)"
          >
            加载更多
          </button>
          <h2>SEP 余额同步</h2>
          <button class="btn btn-secondary" @click="loadEvents(false)"><Icon name="refresh" size="sm" />刷新同步状态</button>
          <div class="table-scroll"><table><thead><tr><th>序号</th><th>来源</th><th>人民币变动</th><th>状态</th><th>重试次数</th><th>具体原因</th><th /></tr></thead><tbody>
            <tr v-for="event in events?.items" :key="event.event_id"><td>{{ event.sequence }}</td><td>{{ event.payload.origin === 'compute' ? '算力平台' : 'SEP' }}</td><td>{{ event.payload.delta_cny }}</td><td>{{ syncStatus(event.status) }}</td><td>{{ event.attempts }}</td><td class="event-error">{{ event.last_error || (event.status === 'pending' && !config.balance_webhook_url ? '尚未配置 SEP 接收地址，事件已保存，可补拉' : '-') }}<small class="block mono">{{ event.event_id }}</small></td><td><button v-if="event.status !== 'delivered'" class="btn btn-secondary btn-sm" :disabled="retrying === event.event_id" @click="retryEvent(event.event_id)"><Icon name="refresh" size="sm" />重试</button></td></tr>
            <tr v-if="!events?.items.length"><td colspan="7" class="empty-state">暂无同步事件</td></tr>
          </tbody></table></div>
          <button v-if="events?.has_more" class="btn btn-secondary" @click="loadEvents(true)">加载更多同步事件</button>
        </div>
      </BaseDialog>
    </div>
  </AppLayout>
</template>

<script setup lang="ts">
import { computed, onMounted, reactive, ref } from "vue";
import { useRoute } from 'vue-router'
import AppLayout from "@/components/layout/AppLayout.vue";
import BaseDialog from "@/components/common/BaseDialog.vue";
import Icon from "@/components/icons/Icon.vue";
import {
  sepAPI,
  type SEPBalance,
  type SEPBill,
  type SEPConfig,
  type SEPEnterprise,
  type SEPLedger,
  type SEPBalanceEvents,
} from "@/api/admin/sep";
import { getAll } from "@/api/admin/groups";
import { useAppStore } from "@/stores/app";
import { extractApiErrorMessage } from "@/utils/apiError";
import type { AdminGroup } from "@/types";
const app = useAppStore();
const route = useRoute()
const rate = computed(() => app.balanceDisplayCnyRate);
const config = reactive<SEPConfig>({
  enabled: false,
  integration_id: "",
  signing_key_id: "",
  signing_public_key: "",
  encryption_key_id: "",
  encryption_public_key: "",
  chat_group_id: 0,
  image_group_id: 0,
  video_group_id: 0,
  balance_webhook_url: '',
  direct_billing_enabled: false,
});
const tabs = [
  { key: "config", label: "对接配置" },
  { key: "enterprises", label: "企业账户" },
];
const purposes = [
  { kind: "chat", label: "对话", field: "chat_group_id" },
  { kind: "image", label: "图片", field: "image_group_id" },
  { kind: "video", label: "视频", field: "video_group_id" },
] as const;
const active = ref("config"),
  loading = ref(true),
  saving = ref(false),
  error = ref(""),
  success = ref(""),
  search = ref(""),
  integrationLocked = ref(false);
const groups = ref<AdminGroup[]>([]),
  enterprises = ref<SEPEnterprise[]>([]);
const detail = ref<{
  enterprise: SEPEnterprise;
  balance: SEPBalance;
  bills: SEPBill[];
} | null>(null);
const detailStatus = ref("ready"),
  detailGroups = reactive<Record<string, number>>({}),
  ledger = ref<SEPLedger | null>(null);
const events = ref<SEPBalanceEvents | null>(null), retrying = ref('')
const eventKey = ref<{ key_id: string; public_key: string; error?: string } | null>(null)
const syncStatus = (status: string) => ({ pending: '待同步', retrying: '同步中', delivered: '同步成功', failed: '同步失败' }[status] || status)
async function loadSigningKey() { try { eventKey.value = await sepAPI.eventSigningKey() } catch (e) { error.value = extractApiErrorMessage(e, '读取通知签名公钥失败') } }
async function loadEvents(more: boolean) {
  if (!detail.value) return
  try { const data = await sepAPI.events(detail.value.enterprise.enterprise_id, more ? events.value?.after_sequence : '0'); if (more && events.value) data.items = [...events.value.items, ...data.items]; events.value = data }
  catch (e) { error.value = extractApiErrorMessage(e, '读取余额同步事件失败') }
}
async function retryEvent(id: string) { retrying.value = id; try { await sepAPI.retryEvent(id); await loadEvents(false) } catch (e) { error.value = extractApiErrorMessage(e, '重试同步事件失败') } finally { retrying.value = '' } }
const filtered = computed(() =>
  enterprises.value.filter((e) =>
    `${e.company_name} ${e.enterprise_id} ${e.admin_email}`
      .toLowerCase()
      .includes(search.value.toLowerCase()),
  ),
);
const availableGroups = (kind: string) =>
  groups.value.filter(
    (g) =>
      g.status === "active" &&
      g.subscription_type !== "subscription" &&
      (kind === "chat" || (g.platform === "openai" && g.allow_image_generation)),
  );
const purposeName = (kind: string) =>
  purposes.find((p) => p.kind === kind)?.label || kind;
const ledgerType = (type: string) =>
  (
    ({
      credit_compute: "转入",
      debit_compute: "退回",
      settlement: "消费结算",
      paid_consumption: "付费消费",
      free_record: "免费消费",
      compute_credit: '算力充值', compute_debit: '算力扣减', compute_refund: '充值退款',
    }) as Record<string, string>
  )[type] || type;
const feeStatus = (value: string) =>
  (
    ({ final: "已确认", pending: "待确认", reconciling: "待核对" }) as Record<
      string,
      string
    >
  )[value] || value;
async function load() {
  loading.value = true;
  error.value = "";
  try {
    const [c, g, e] = await Promise.all([
      sepAPI.config(),
      getAll(),
      sepAPI.enterprises(),
    ]);
    Object.assign(config, c);
    integrationLocked.value = !!c.integration_id;
    groups.value = g;
    enterprises.value = e;
  } catch (e) {
    error.value = extractApiErrorMessage(e, "读取 SEP 配置失败");
  } finally {
    loading.value = false;
  }
}
async function save() {
  saving.value = true;
  error.value = "";
  success.value = "";
  try {
    Object.assign(config, await sepAPI.save(config));
    integrationLocked.value = !!config.integration_id;
    success.value = "配置已保存";
  } catch (e) {
    error.value = extractApiErrorMessage(e, "保存 SEP 配置失败");
  } finally {
    saving.value = false;
  }
}
async function openEnterprise(id: string) {
  error.value = "";
  try {
    detail.value = await sepAPI.detail(id);
    detailStatus.value = detail.value.enterprise.status;
    for (const c of detail.value.enterprise.credentials || [])
      detailGroups[c.type] = c.group_id;
    ledger.value = null;
    await loadLedger(false);
    events.value = null;
    await loadEvents(false);
  } catch (e) {
    error.value = extractApiErrorMessage(e, "读取企业账户失败");
  }
}
async function saveEnterprise() {
  if (!detail.value) return;
  saving.value = true;
  try {
    detail.value = await sepAPI.update(
      detail.value.enterprise.enterprise_id,
      detailStatus.value,
      detailGroups,
    );
    enterprises.value = await sepAPI.enterprises();
    success.value = "企业设置已保存";
  } catch (e) {
    error.value = extractApiErrorMessage(e, "保存企业设置失败");
  } finally {
    saving.value = false;
  }
}
async function loadLedger(more: boolean) {
  if (!detail.value) return;
  try {
    const data = await sepAPI.ledger(
      detail.value.enterprise.enterprise_id,
      more ? ledger.value?.after_sequence : "0",
      more ? ledger.value?.snapshot_sequence : "0",
    );
    if (more && ledger.value)
      data.items = [...ledger.value.items, ...data.items];
    ledger.value = data;
  } catch (e) {
    error.value = extractApiErrorMessage(e, "读取流水失败");
  }
}
onMounted(async () => { await load(); if (typeof route.query.enterprise === 'string') { active.value = 'enterprises'; await openEnterprise(route.query.enterprise) } });
</script>

<style scoped>
.event-error { min-width: 220px; max-width: 440px; white-space: normal; overflow-wrap: anywhere; }
.enterprise-detail .table-scroll table { min-width: 680px; }
.enterprise-detail .table-scroll { min-width: 0; }
.sep-page {
  max-width: 1180px;
  margin: auto;
  color: var(--text-primary, #172126);
}
.sep-heading,
.sep-heading > div,
.section-label,
.enterprise-toolbar,
.detail-actions {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: 16px;
}
.sep-heading > div {
  justify-content: flex-start;
}
.sep-heading h1 {
  font-size: 24px;
  font-weight: 700;
}
.sep-tabs {
  display: flex;
  gap: 24px;
  border-bottom: 1px solid #dbe1e5;
  margin-top: 24px;
}
.sep-tabs button {
  padding: 12px 2px;
  border-bottom: 2px solid transparent;
  color: #64717a;
}
.sep-tabs .selected {
  border-color: #d63d0a;
  color: #d63d0a;
}
.config-section {
  padding: 24px 0;
  border-bottom: 1px solid #dbe1e5;
}
.config-section h2,
.enterprise-detail h2 {
  font-size: 16px;
  font-weight: 650;
  margin-bottom: 16px;
}
.section-label h2 {
  margin: 0;
}
.section-label {
  margin-bottom: 20px;
}
.enable-label {
  display: flex;
  align-items: center;
  gap: 8px;
  font-size: 14px;
}
.field-grid {
  display: grid;
  grid-template-columns: minmax(0, 1fr) minmax(0, 1fr);
  gap: 24px;
  align-items: end;
}
.group-grid {
  display: grid;
  grid-template-columns: repeat(3, minmax(0, 1fr));
  gap: 20px;
}
label:not(.enable-label) {
  display: grid;
  gap: 8px;
  font-size: 13px;
  color: #53616c;
}
.input {
  width: 100%;
  min-width: 0;
}
.rate-readout {
  display: grid;
  gap: 7px;
  font-size: 13px;
}
.rate-readout strong {
  font-size: 16px;
  font-weight: 600;
}
.rate-readout a {
  display: flex;
  gap: 6px;
  align-items: center;
  color: #cf380b;
}
.algorithm {
  font-size: 12px;
  color: #63747f;
  align-self: center;
}
.pem-label {
  margin-top: 16px;
}
.pem,
.mono {
  font-family: ui-monospace, SFMono-Regular, Consolas, monospace;
}
.pem {
  font-size: 12px;
  resize: vertical;
  overflow-wrap: anywhere;
}
.save-bar {
  display: flex;
  justify-content: flex-end;
  padding: 20px 0;
}
.state {
  font-size: 12px;
  border-radius: 4px;
  padding: 3px 7px;
  white-space: nowrap;
}
.state.on {
  background: #e2f5eb;
  color: #176740;
}
.state.off {
  background: #eceff1;
  color: #606d76;
}
.enterprise-section {
  padding-top: 24px;
}
.enterprise-toolbar {
  margin-bottom: 18px;
  font-size: 13px;
  color: #64717a;
}
.enterprise-toolbar input {
  max-width: 380px;
}
.table-scroll {
  overflow-x: auto;
}
table {
  width: 100%;
  border-collapse: collapse;
  font-size: 13px;
}
th {
  text-align: left;
  color: #687681;
  font-weight: 500;
  white-space: nowrap;
}
td,
th {
  padding: 13px 12px;
  border-bottom: 1px solid #e2e7eb;
}
td {
  overflow-wrap: anywhere;
}
.empty-state {
  text-align: center;
  color: #76838d;
  padding: 44px 12px;
}
.feedback {
  display: flex;
  align-items: center;
  justify-content: space-between;
  padding: 12px;
  margin-top: 16px;
  font-size: 13px;
}
.feedback.error {
  background: #fff0ec;
  color: #a52f16;
}
.feedback.success {
  background: #e9f6ed;
  color: #216240;
}
.enterprise-detail {
  display: grid;
  gap: 20px;
}
.balance-strip {
  display: grid;
  grid-template-columns: repeat(3, minmax(0, 1fr));
  gap: 12px;
  border-block: 1px solid #e2e7eb;
  padding: 16px 0;
}
.balance-strip dt {
  font-size: 12px;
  color: #6a7780;
}
.balance-strip dd {
  font-size: 16px;
  font-weight: 650;
  margin-top: 5px;
  overflow-wrap: anywhere;
}
.key-row {
  display: grid;
  grid-template-columns: minmax(0, 1fr) minmax(0, 1fr);
  gap: 16px;
  align-items: center;
  padding: 12px 0;
  border-bottom: 1px solid #e2e7eb;
}
.key-row small {
  display: block;
  font-size: 11px;
  color: #74818b;
  margin-top: 4px;
}
.detail-status label {
  max-width: 220px;
}
.dark .sep-page {
  color: #e7edef;
}
.dark .config-section,
.dark .sep-tabs,
.dark td,
.dark th,
.dark .balance-strip,
.dark .key-row {
  border-color: #37434d;
}
.dark label:not(.enable-label) {
  color: #bac7d0;
}
@media (max-width: 640px) {
  .sep-heading h1 {
    font-size: 20px;
  }
  .field-grid,
  .group-grid {
    grid-template-columns: 1fr;
    gap: 16px;
  }
  .section-label {
    align-items: flex-start;
  }
  .balance-strip {
    grid-template-columns: 1fr 1fr;
  }
  .sep-heading {
    gap: 10px;
  }
  .sep-heading > div {
    gap: 8px;
  }
  .sep-heading .btn {
    padding: 8px;
  }
  .enterprise-toolbar {
    align-items: flex-start;
    flex-direction: column;
  }
  .detail-actions {
    flex-wrap: wrap;
  }
  .key-row {
    grid-template-columns: 1fr;
  }
  .algorithm {
    overflow-wrap: anywhere;
  }
}
</style>
