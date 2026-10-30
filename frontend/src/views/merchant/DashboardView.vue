<template>
  <AppLayout>
    <div class="space-y-6">
      <header class="flex flex-wrap items-start justify-between gap-4">
        <div>
          <div class="flex items-center gap-2">
            <h1 class="text-2xl font-semibold text-gray-900 dark:text-white">
              {{ t('merchant.dashboard.title') }}
            </h1>
            <span class="badge badge-primary">{{ t('merchant.dashboard.badge') }}</span>
          </div>
          <p class="mt-1 text-sm text-gray-500 dark:text-dark-400">
            {{ dashboard?.name || t('merchant.dashboard.description') }}
          </p>
        </div>
      </header>

      <!-- 商户后台导航：概览与提现已接入，其余任务后续补齐。 -->
      <nav class="card flex flex-wrap items-center gap-1 p-1.5" aria-label="merchant console">
        <RouterLink
          to="/merchant/dashboard"
          class="rounded-lg px-3 py-1.5 text-sm font-medium text-gray-600 hover:bg-gray-100 dark:text-dark-300 dark:hover:bg-dark-800"
        >
          {{ t('merchant.nav.overview') }}
        </RouterLink>
        <RouterLink
          to="/merchant/withdrawals"
          class="rounded-lg px-3 py-1.5 text-sm font-medium text-primary-700 hover:bg-primary-50 dark:text-primary-300 dark:hover:bg-primary-900/30"
        >
          {{ t('merchant.nav.withdrawals') }}
        </RouterLink>
        <span
          v-for="item in navPlaceholders"
          :key="item.key"
          class="inline-flex cursor-not-allowed items-center gap-2 rounded-lg px-3 py-1.5 text-sm text-gray-400 dark:text-dark-500"
          :title="t('merchant.nav.comingSoon')"
          aria-disabled="true"
        >
          {{ t(item.key) }}
          <span class="rounded bg-gray-100 px-1.5 py-0.5 text-[10px] font-medium text-gray-500 dark:bg-dark-800 dark:text-dark-400">
            {{ t('merchant.nav.comingSoon') }}
          </span>
        </span>
      </nav>

      <div v-if="loading" class="flex justify-center py-16">
        <div class="h-8 w-8 animate-spin rounded-full border-2 border-primary-500 border-t-transparent"></div>
      </div>

      <div v-else-if="loadFailed" class="card p-8 text-center">
        <p class="text-gray-600 dark:text-dark-300">{{ t('merchant.loadFailed') }}</p>
        <button class="btn btn-secondary mt-4" @click="load">{{ t('merchant.retry') }}</button>
      </div>

      <template v-else-if="dashboard">
        <section class="grid gap-4 md:grid-cols-3">
          <article class="card p-5">
            <p class="text-sm text-gray-500 dark:text-dark-400">{{ t('merchant.stats.name') }}</p>
            <p class="mt-2 truncate text-xl font-semibold text-gray-900 dark:text-white">
              {{ dashboard.name }}
            </p>
            <p class="mt-1 text-xs text-gray-400 dark:text-dark-500">#{{ dashboard.merchant_id }}</p>
          </article>
          <article class="card p-5">
            <p class="text-sm text-gray-500 dark:text-dark-400">{{ t('merchant.stats.commissionRate') }}</p>
            <p class="mt-2 text-xl font-semibold text-gray-900 dark:text-white">
              {{ commissionPercent }}
            </p>
          </article>
          <article class="card p-5">
            <p class="text-sm text-gray-500 dark:text-dark-400">{{ t('merchant.stats.payableBalance') }}</p>
            <p class="mt-2 text-xl font-semibold text-gray-900 dark:text-white">
              {{ formatCurrency(dashboard.payable_balance) }}
            </p>
            <p class="mt-1 text-xs text-gray-400 dark:text-dark-500">
              {{ t('merchant.stats.payableHint') }}
            </p>
          </article>
        </section>

        <section class="card p-5">
          <div class="flex flex-wrap items-start justify-between gap-3">
            <div>
              <h2 class="font-semibold text-gray-900 dark:text-white">{{ t('merchant.usage.title') }}</h2>
              <p class="mt-1 text-xs text-gray-400 dark:text-dark-500">
                {{ trend?.start_date }} — {{ trend?.end_date }}
              </p>
            </div>
            <div class="flex items-center gap-4 text-xs text-gray-500 dark:text-dark-400">
              <span class="inline-flex items-center gap-1.5">
                <span class="h-2 w-2 rounded-full bg-primary-500"></span>
                {{ t('merchant.usage.requests') }}
              </span>
              <span class="inline-flex items-center gap-1.5">
                <span class="h-2 w-2 rounded-full bg-emerald-500"></span>
                {{ t('merchant.usage.netPayable') }}
              </span>
            </div>
          </div>

          <div v-if="!hasUsage" class="py-12 text-center text-sm text-gray-500 dark:text-dark-400">
            {{ t('merchant.usage.empty') }}
          </div>
          <div v-else class="mt-4 overflow-x-auto">
            <svg
              class="h-56 w-full min-w-[520px]"
              viewBox="0 0 640 220"
              role="img"
              :aria-label="t('merchant.usage.title')"
            >
              <g v-for="grid in gridLines" :key="grid.value">
                <line
                  :x1="chartPadding.left"
                  :x2="chartWidth - chartPadding.right"
                  :y1="grid.y"
                  :y2="grid.y"
                  stroke="currentColor"
                  class="text-gray-200 dark:text-dark-700"
                  stroke-width="1"
                />
                <text
                  :x="chartPadding.left - 6"
                  :y="grid.y + 4"
                  text-anchor="end"
                  class="fill-gray-400 text-[10px] dark:fill-dark-500"
                >
                  {{ grid.label }}
                </text>
              </g>

              <polyline
                :points="requestsPolyline"
                fill="none"
                stroke="currentColor"
                class="text-primary-500"
                stroke-width="2.5"
                stroke-linejoin="round"
                stroke-linecap="round"
              />
              <g v-for="point in requestsPoints" :key="`req-${point.index}`">
                <circle
                  :cx="point.x"
                  :cy="point.y"
                  r="3.5"
                  fill="currentColor"
                  class="text-primary-500"
                >
                  <title>{{ point.date }}: {{ point.requests }}</title>
                </circle>
                <text
                  :x="point.x"
                  :y="chartHeight - chartPadding.bottom + 16"
                  text-anchor="middle"
                  class="fill-gray-400 text-[10px] dark:fill-dark-500"
                >
                  {{ shortDate(point.date) }}
                </text>
              </g>

              <polyline
                v-if="hasNetPayable"
                :points="netPolyline"
                fill="none"
                stroke="currentColor"
                class="text-emerald-500"
                stroke-width="2"
                stroke-dasharray="4 3"
                stroke-linejoin="round"
              />
            </svg>
          </div>

          <div class="mt-4 grid gap-3 border-t border-gray-100 pt-4 sm:grid-cols-3 dark:border-dark-800">
            <div>
              <p class="text-xs text-gray-500 dark:text-dark-400">{{ t('merchant.usage.totalRequests') }}</p>
              <p class="mt-1 text-lg font-semibold text-gray-900 dark:text-white">
                {{ formatNumber(totals.requests) }}
              </p>
            </div>
            <div>
              <p class="text-xs text-gray-500 dark:text-dark-400">{{ t('merchant.usage.totalGross') }}</p>
              <p class="mt-1 text-lg font-semibold text-gray-900 dark:text-white">
                {{ formatCurrency(totals.gross) }}
              </p>
            </div>
            <div>
              <p class="text-xs text-gray-500 dark:text-dark-400">{{ t('merchant.usage.totalNet') }}</p>
              <p class="mt-1 text-lg font-semibold text-gray-900 dark:text-white">
                {{ formatCurrency(totals.net) }}
              </p>
            </div>
          </div>
        </section>
      </template>
    </div>
  </AppLayout>
</template>

<script setup lang="ts">
import { computed, onMounted, ref } from 'vue'
import { useI18n } from 'vue-i18n'
import AppLayout from '@/components/layout/AppLayout.vue'
import { merchantAPI, type MerchantDashboard, type MerchantUsageTrend } from '@/api/merchant'
import { formatCurrency, formatNumber } from '@/utils/format'

const { t } = useI18n()

const dashboard = ref<MerchantDashboard | null>(null)
const trend = ref<MerchantUsageTrend | null>(null)
const loading = ref(false)
const loadFailed = ref(false)

const navPlaceholders = [
  { key: 'merchant.nav.groups' },
  { key: 'merchant.nav.settlements' },
  { key: 'merchant.nav.disputes' },
] as const

const commissionPercent = computed(() => {
  const rate = dashboard.value?.commission_rate
  if (rate === undefined || rate === null) return '-'
  return `${(rate * 100).toFixed(2)}%`
})

const points = computed(() => trend.value?.items ?? [])
const hasUsage = computed(() => points.value.some((point) => point.requests > 0 || point.net_payable > 0))
const hasNetPayable = computed(() => points.value.some((point) => point.net_payable > 0))

const totals = computed(() => {
  return points.value.reduce(
    (acc, point) => ({
      requests: acc.requests + point.requests,
      gross: acc.gross + point.gross_cost,
      net: acc.net + point.net_payable,
    }),
    { requests: 0, gross: 0, net: 0 },
  )
})

// 请求数与应付金额量级不同，分别按各自最大值缩放；图例与标题标明两条曲线。
const chartWidth = 640
const chartHeight = 220
const chartPadding = { top: 16, right: 16, bottom: 28, left: 48 }
const plotWidth = chartWidth - chartPadding.left - chartPadding.right
const plotHeight = chartHeight - chartPadding.top - chartPadding.bottom

function scaleY(value: number, max: number): number {
  if (max <= 0) return chartPadding.top + plotHeight
  return chartPadding.top + (1 - value / max) * plotHeight
}

function xAt(index: number): number {
  const count = Math.max(points.value.length, 1)
  if (count === 1) return chartPadding.left + plotWidth / 2
  return chartPadding.left + (index / (count - 1)) * plotWidth
}

const maxRequests = computed(() => Math.max(1, ...points.value.map((point) => point.requests)))
const maxNet = computed(() => Math.max(0, ...points.value.map((point) => point.net_payable)))

const requestsPoints = computed(() =>
  points.value.map((point, index) => ({
    index,
    date: point.date,
    requests: point.requests,
    x: xAt(index),
    y: scaleY(point.requests, maxRequests.value),
  })),
)

const requestsPolyline = computed(() =>
  requestsPoints.value.map((point) => `${point.x},${point.y}`).join(' '),
)

const netPolyline = computed(() =>
  points.value
    .map((point, index) => `${xAt(index)},${scaleY(point.net_payable, maxNet.value)}`)
    .join(' '),
)

const gridLines = computed(() =>
  [0, 0.5, 1].map((ratio) => ({
    value: ratio,
    y: chartPadding.top + (1 - ratio) * plotHeight,
    label: formatNumber(Math.round(maxRequests.value * ratio)),
  })),
)

function shortDate(date: string): string {
  return date.slice(5)
}

async function load(): Promise<void> {
  loading.value = true
  loadFailed.value = false
  try {
    const [dashboardData, trendData] = await Promise.all([
      merchantAPI.getMerchantDashboard(),
      merchantAPI.getMerchantDailyUsage(7),
    ])
    dashboard.value = dashboardData
    trend.value = trendData
  } catch (error) {
    console.error('Failed to load merchant console:', error)
    loadFailed.value = true
  } finally {
    loading.value = false
  }
}

onMounted(load)
</script>
