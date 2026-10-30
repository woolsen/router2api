/**
 * 商户后台 API
 *
 * 所有接口的 merchant_id 都由服务端按已认证用户注入，
 * 前端不传、也不允许覆盖商户身份。
 */

import { apiClient } from './client'

/** 商户概览：名称、抽成率与未打款应付余额。 */
export interface MerchantDashboard {
  merchant_id: number
  name: string
  commission_rate: number
  payable_balance: number
}

/** 单日计费用量；金额口径与结算单一致（仅钱包计费）。 */
export interface MerchantDailyUsagePoint {
  date: string
  requests: number
  gross_cost: number
  commission: number
  net_payable: number
}

export interface MerchantUsageTrend {
  days: number
  start_date: string
  end_date: string
  items: MerchantDailyUsagePoint[]
}

export async function getMerchantDashboard(): Promise<MerchantDashboard> {
  const { data } = await apiClient.get<MerchantDashboard>('/merchant/me')
  return data
}

export async function getMerchantDailyUsage(days = 7): Promise<MerchantUsageTrend> {
  const { data } = await apiClient.get<MerchantUsageTrend>('/merchant/usage/daily', {
    params: { days },
  })
  return data
}

/** 提现单状态机：requested 待审核 → approved 待打款 → paid 已打款；requested/approved → rejected。 */
export type MerchantWithdrawalStatus = 'requested' | 'approved' | 'paid' | 'rejected'

/** 一笔商户提现。金额只在服务端钱包账本里流转，前端不参与计算。 */
export interface MerchantWithdrawal {
  id: number
  merchant_id: number
  amount: number
  status: MerchantWithdrawalStatus
  proof_path?: string
  proof_sha256?: string
  approved_by?: string
  requested_at: string
  paid_at?: string
}

/** 可提现余额与提现记录。 */
export interface MerchantWithdrawals {
  available: number
  minimum: number
  items: MerchantWithdrawal[]
}

export async function getMerchantWithdrawals(): Promise<MerchantWithdrawals> {
  const { data } = await apiClient.get<MerchantWithdrawals>('/merchant/withdrawals')
  return data
}

export async function requestMerchantWithdrawal(amount: number): Promise<MerchantWithdrawal> {
  const { data } = await apiClient.post<MerchantWithdrawal>('/merchant/withdrawals', { amount })
  return data
}

export const merchantAPI = {
  getMerchantDashboard,
  getMerchantDailyUsage,
  getMerchantWithdrawals,
  requestMerchantWithdrawal,
}

export default merchantAPI
