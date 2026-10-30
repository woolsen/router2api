import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { flushPromises, mount } from '@vue/test-utils'
import MarketView from '../MarketView.vue'
import type { MarketGroup, MarketGroupStats, MarketStatsWindow } from '@/api/market'

const { getMarketGroups } = vi.hoisted(() => ({
  getMarketGroups: vi.fn(),
}))

vi.mock('@/api/market', () => ({ getMarketGroups }))
vi.mock('vue-router', () => ({ useRouter: () => ({ push: vi.fn() }) }))
vi.mock('vue-i18n', async () => {
  const actual = await vi.importActual<typeof import('vue-i18n')>('vue-i18n')
  return { ...actual, useI18n: () => ({ t: (key: string) => key }) }
})

function window(overrides: Partial<MarketStatsWindow> = {}): MarketStatsWindow {
  return {
    requests: 0,
    upstream_failures: 0,
    success_rate: 1,
    request_error_rate: 0,
    ttft_samples: 0,
    p50_ttft_ms: null,
    p95_ttft_ms: null,
    cache_hit_rate: null,
    ...overrides,
  }
}

function group(stats: MarketGroupStats | null): MarketGroup {
  return {
    id: 1,
    name: 'Merchant A',
    platform: 'anthropic',
    rate_multiplier: 0.2,
    rate_is_estimate: false,
    peak_rate: null,
    models: ['claude-sonnet-4-5'],
    status: 'active',
    stats,
    sample_size: stats?.last_24h?.requests ?? 0,
    degraded: false,
    degraded_reason: '',
    as_of: stats ? '2026-10-29T12:00:00Z' : null,
  }
}

async function mountView(stats: MarketGroupStats | null) {
  getMarketGroups.mockResolvedValue([group(stats)])
  const wrapper = mount(MarketView, {
    global: {
      stubs: {
        AppLayout: { template: '<div><slot /></div>' },
        PlatformIcon: true,
      },
    },
  })
  await flushPromises()
  return wrapper
}

describe('MarketView metrics', () => {
  beforeEach(() => {
    vi.clearAllMocks()
    vi.spyOn(console, 'error').mockImplementation(() => {})
  })

  afterEach(() => {
    vi.restoreAllMocks()
  })

  it('reads the nested 24h window for requests/success and the 1h window for TTFT P95', async () => {
    const wrapper = await mountView({
      last_hour: window({ requests: 30, p95_ttft_ms: 850 }),
      last_24h: window({ requests: 120, success_rate: 0.95 }),
    })

    const requests = wrapper.get('[data-test="market-metric-requests"]')
    const success = wrapper.get('[data-test="market-metric-success"]')
    const ttft = wrapper.get('[data-test="market-metric-ttft"]')

    expect(requests.text()).toContain('market.metrics.requests24h')
    expect(requests.text()).toContain('120')
    expect(success.text()).toContain('market.metrics.successRate24h')
    expect(success.text()).toContain('95.0%')
    expect(ttft.text()).toContain('market.metrics.ttftP951h')
    expect(ttft.text()).toContain('850 ms')
    wrapper.unmount()
  })

  it('shows "insufficient" for a null TTFT percentile while keeping available 24h metrics', async () => {
    const wrapper = await mountView({
      last_hour: window({ requests: 30, p95_ttft_ms: null }),
      last_24h: window({ requests: 120, success_rate: 0.9 }),
    })

    expect(wrapper.get('[data-test="market-metric-requests"]').text()).toContain('120')
    expect(wrapper.get('[data-test="market-metric-success"]').text()).toContain('90.0%')
    const ttft = wrapper.get('[data-test="market-metric-ttft"]')
    expect(ttft.text()).toContain('market.insufficient')
    expect(ttft.text()).not.toContain('ms')
    wrapper.unmount()
  })

  it('shows "insufficient" for every metric when stats is null', async () => {
    const wrapper = await mountView(null)

    for (const selector of [
      '[data-test="market-metric-requests"]',
      '[data-test="market-metric-success"]',
      '[data-test="market-metric-ttft"]',
    ]) {
      expect(wrapper.get(selector).text()).toContain('market.insufficient')
    }
    wrapper.unmount()
  })
})
