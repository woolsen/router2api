/**
 * Market（货架）API
 *
 * 只读地列出入驻商户上架的分组，用于挑选分组后跳转密钥创建流程。
 * 后端已按 rate_multiplier 升序返回，前端不再重排。
 */

import { apiClient } from './client'
import type { GroupPlatform } from '@/types'

/**
 * 一档小时窗口的指标，对应后端 marketStatsWindow。
 *
 * 计数、成功率与缓存率由小时原始值精确求和；P50/P95 首包分位数只在窗口
 * 恰好一个整点桶时有意义，跨桶或没有流式样本时为 null，必须显示「数据不足」，
 * 不能用 0 冒充观测值。
 */
export interface MarketStatsWindow {
  requests: number
  upstream_failures: number
  success_rate: number
  request_error_rate: number
  ttft_samples: number
  p50_ttft_ms: number | null
  p95_ttft_ms: number | null
  cache_hit_rate: number | null
}

/**
 * 货架的 1h + 24h 两档指标，对应后端 marketGroupStats。
 * 24h 样本不足 50 条时整个 stats 为 null；某一档窗口无数据时该档为 null。
 */
export interface MarketGroupStats {
  last_hour: MarketStatsWindow | null
  last_24h: MarketStatsWindow | null
}

export interface MarketPeakRate {
  start: string
  end: string
  multiplier: number
  /** 本次请求时刻是否落在高峰窗口内（与计费同一判定）。 */
  active_now: boolean
}

export interface MarketGroup {
  id: number
  name: string
  platform: GroupPlatform
  /**
   * 当前用户实际生效的 token 倍率（用户专属倍率优先于分组基础倍率）。
   * 峰时倍率不并入此值，见 peak_rate。
   */
  rate_multiplier: number
  /** true 表示服务端拿不到用户专属倍率，展示的只是基础倍率，以实际计费为准。 */
  rate_is_estimate: boolean
  /** 高峰时段倍率信息；未启用时为 null。 */
  peak_rate: MarketPeakRate | null
  models: string[]
  status: string
  /** 24h 样本不足 50 条时为 null；某一档窗口无数据时该档为 null。 */
  stats: MarketGroupStats | null
  /** 24h 档的实际请求数；即使 stats 为 null 也如实返回。 */
  sample_size: number
  /** 只做展示标记，分组仍可正常使用。 */
  degraded: boolean
  degraded_reason: string
  /** 最后一档指标的聚合截止时间；没有聚合数据时为 null。 */
  as_of: string | null
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
