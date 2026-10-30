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

export const merchantAPI = {
  getMerchantDashboard,
  getMerchantDailyUsage,
}

export default merchantAPI
