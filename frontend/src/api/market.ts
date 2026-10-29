/**
 * Market（货架）API
 *
 * 只读地列出入驻商户上架的分组，用于挑选分组后跳转密钥创建流程。
 * 后端已按 rate_multiplier 升序返回，前端不再重排。
 */

import { apiClient } from './client'
import type { GroupPlatform } from '@/types'

/**
 * 分组指标负载。M1 后端恒返回 null（指标由 M2 的 request_metrics 提供，
 * usage_logs 只记成功请求，拼出来的成功率会误导用户）。
 *
 * 字段名是 M1 的占位约定，M2 落地时以 request_metrics 契约为准；为 null 时
 * 页面不会渲染任何数字，只显示「数据不足」。
 */
export type MarketGroupStats = Record<string, number>

export interface MarketGroup {
  id: number
  name: string
  platform: GroupPlatform
  rate_multiplier: number
  models: string[]
  status: string
  /** M1 恒为 null；客户端必须能处理 null。 */
  stats: MarketGroupStats | null
}

/** 货架分组列表（已按倍率升序）。 */
export async function getMarketGroups(): Promise<MarketGroup[]> {
  const { data } = await apiClient.get<MarketGroup[] | null>('/market/groups')
  return data ?? []
}

export const marketAPI = {
  getMarketGroups
}

export default marketAPI
