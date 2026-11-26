import { apiClient } from "../client";
export interface SEPConfig {
  enabled: boolean;
  integration_id: string;
  signing_key_id: string;
  signing_public_key: string;
  encryption_key_id: string;
  encryption_public_key: string;
  chat_group_id: number;
  image_group_id: number;
  video_group_id: number;
  balance_webhook_url?: string;
  direct_billing_enabled?: boolean;
}
export interface SEPEnterprise {
  enterprise_id: string;
  compute_account_id: number;
  company_name: string;
  admin_email: string;
  status: "ready" | "disabled";
  credentials?: {
    type: "chat" | "image" | "video";
    credential_id: string;
    credential_version: number;
    group_id: number;
    masked_key: string;
  }[];
}
export interface SEPBalance {
  total_paid_cny: string;
  available_paid_cny: string;
  frozen_paid_cny: string;
  sequence: string;
}
export interface SEPBill {
  billing_origin?: string;
  billing_error?: string;
  execution_id: string;
  kind: string;
  model_id: string;
  execution_status: string;
  fee_status: string;
  gross_cny: string | null;
  settlement_id: string;
  created_at: string;
}
export interface SEPBalanceEvent {
  event_id: string; sequence: string; status: string; attempts: number; last_error: string; next_attempt_at: string;
  payload: { origin: string; type: string; delta_cny: string; available_paid_cny: string; business_ref?: string; execution_id?: string; transfer_id?: string };
}
export interface SEPBalanceEvents { items: SEPBalanceEvent[]; after_sequence: string; has_more: boolean }
export interface SEPLedger {
  items: {
    ledger_entry_id: string;
    sequence: string;
    type: string;
    amount_cny: string;
    free_cny: string;
    transfer_id: string;
    execution_id: string;
    effective_at: string;
  }[];
  snapshot_sequence: string;
  after_sequence: string;
  has_more: boolean;
}
export const sepAPI = {
  async events(id: string, after = '0') { return (await apiClient.get<SEPBalanceEvents>(`/admin/sep/enterprises/${encodeURIComponent(id)}/balance-events`, { params: { after_sequence: after } })).data },
  async retryEvent(id: string) { return (await apiClient.post(`/admin/sep/balance-events/${encodeURIComponent(id)}/retry`)).data },
  async eventSigningKey() { return (await apiClient.get<{ key_id: string; public_key: string; error?: string }>('/admin/sep/balance-events/signing-key')).data },
  async config() {
    return (await apiClient.get<SEPConfig>("/admin/sep/config")).data;
  },
  async save(config: SEPConfig) {
    return (await apiClient.put<SEPConfig>("/admin/sep/config", config)).data;
  },
  async enterprises() {
    return (await apiClient.get<SEPEnterprise[]>("/admin/sep/enterprises"))
      .data;
  },
  async detail(id: string) {
    return (
      await apiClient.get<{
        enterprise: SEPEnterprise;
        balance: SEPBalance;
        bills: SEPBill[];
      }>(`/admin/sep/enterprises/${encodeURIComponent(id)}`)
    ).data;
  },
  async update(id: string, status: string, groups: Record<string, number>) {
    return (
      await apiClient.put<{
        enterprise: SEPEnterprise;
        balance: SEPBalance;
        bills: SEPBill[];
      }>(`/admin/sep/enterprises/${encodeURIComponent(id)}`, { status, groups })
    ).data;
  },
  async ledger(id: string, after = "0", snapshot = "0") {
    return (
      await apiClient.get<SEPLedger>(
        `/admin/sep/enterprises/${encodeURIComponent(id)}/ledger`,
        { params: { after_sequence: after, snapshot_sequence: snapshot } },
      )
    ).data;
  },
};
