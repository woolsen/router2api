<template>
  <AppLayout>
    <div class="space-y-6">
      <header>
        <h1 class="text-2xl font-semibold text-gray-900 dark:text-white">
          {{ t('market.title') }}
        </h1>
        <p class="mt-1 text-sm text-gray-500 dark:text-dark-400">
          {{ t('market.description') }}
        </p>
      </header>

      <div v-if="loading" class="flex justify-center py-16">
        <div
          class="h-8 w-8 animate-spin rounded-full border-2 border-primary-500 border-t-transparent"
        ></div>
      </div>

      <div v-else-if="loadFailed" class="card p-8 text-center">
        <p class="text-gray-600 dark:text-dark-300">{{ t('market.loadFailed') }}</p>
        <button class="btn btn-secondary mt-4" @click="load">
          {{ t('market.retry') }}
        </button>
      </div>

      <div v-else-if="groups.length === 0" class="card p-12 text-center">
        <h3 class="mb-2 text-lg font-semibold text-gray-900 dark:text-white">
          {{ t('market.empty') }}
        </h3>
        <p class="text-gray-500 dark:text-dark-400">{{ t('market.emptyDesc') }}</p>
      </div>

      <div v-else class="grid gap-4 md:grid-cols-2 xl:grid-cols-3">
        <article
          v-for="group in groups"
          :key="group.id"
          class="card flex flex-col p-5"
          data-test="market-group-card"
        >
          <div class="flex items-start justify-between gap-3">
            <div class="min-w-0">
              <h2 class="truncate font-semibold text-gray-900 dark:text-white">
                {{ group.name }}
              </h2>
              <span
                :class="[
                  'mt-1 inline-flex items-center gap-1.5 rounded-md border px-2 py-0.5 text-[11px] font-medium',
                  platformBadgeClass(group.platform)
                ]"
              >
                <PlatformIcon :platform="group.platform" size="xs" />
                {{ platformLabel(group.platform) }}
              </span>
            </div>
            <div class="shrink-0 text-right">
              <div class="text-[11px] text-gray-500 dark:text-dark-400">
                {{ t('market.rate') }}
              </div>
              <div class="text-lg font-semibold text-gray-900 dark:text-white">
                ×{{ group.rate_multiplier }}
              </div>
            </div>
          </div>

          <div class="mt-4 flex-1">
            <div class="text-[11px] text-gray-500 dark:text-dark-400">
              {{ t('market.models', { count: group.models.length }) }}
            </div>
            <div class="mt-2 flex flex-wrap gap-1.5">
              <span
                v-for="model in group.models"
                :key="model"
                class="rounded-md bg-gray-100 px-2 py-0.5 text-xs text-gray-600 dark:bg-dark-700 dark:text-dark-300"
              >
                {{ model }}
              </span>
              <span
                v-if="group.models.length === 0"
                class="text-xs text-gray-400 dark:text-dark-500"
              >
                {{ t('market.noModels') }}
              </span>
            </div>
          </div>

          <!-- M1 三项指标恒为「数据不足」（stats 为 null）；不展示任何推算数字。 -->
          <div
            class="mt-4 grid grid-cols-3 gap-2 rounded-lg bg-gray-50 p-3 text-center dark:bg-dark-700/40"
          >
            <div>
              <div class="text-[11px] text-gray-500 dark:text-dark-400">
                {{ t('market.metrics.requests') }}
              </div>
              <div class="mt-1 text-xs font-medium text-gray-400 dark:text-dark-500">
                {{ metricText('requests', group) }}
              </div>
            </div>
            <div>
              <div class="text-[11px] text-gray-500 dark:text-dark-400">
                {{ t('market.metrics.successRate') }}
              </div>
              <div class="mt-1 text-xs font-medium text-gray-400 dark:text-dark-500">
                {{ metricText('success_rate', group) }}
              </div>
            </div>
            <div>
              <div class="text-[11px] text-gray-500 dark:text-dark-400">
                {{ t('market.metrics.latency') }}
              </div>
              <div class="mt-1 text-xs font-medium text-gray-400 dark:text-dark-500">
                {{ metricText('avg_latency_ms', group) }}
              </div>
            </div>
          </div>

          <div class="mt-5 flex justify-end">
            <button
              class="btn btn-primary"
              data-test="market-use-group"
              @click="useGroup(group)"
            >
              {{ t('market.use') }}
            </button>
          </div>
        </article>
      </div>
    </div>
  </AppLayout>
</template>

<script setup lang="ts">
import { onMounted, ref } from 'vue'
import { useI18n } from 'vue-i18n'
import { useRouter } from 'vue-router'
import AppLayout from '@/components/layout/AppLayout.vue'
import PlatformIcon from '@/components/common/PlatformIcon.vue'
import { getMarketGroups, type MarketGroup } from '@/api/market'
import { platformBadgeClass, platformLabel } from '@/utils/platformColors'

const { t } = useI18n()
const router = useRouter()

const groups = ref<MarketGroup[]>([])
const loading = ref(true)
const loadFailed = ref(false)

const load = async () => {
  loading.value = true
  loadFailed.value = false
  try {
    groups.value = await getMarketGroups()
  } catch (error) {
    console.error('Failed to load market groups:', error)
    loadFailed.value = true
  } finally {
    loading.value = false
  }
}

/**
 * 指标文案：stats 为 null（M1 的常态）时显示「数据不足」，绝不回落到 0 或估算值。
 * 字段名是 M2 接入 request_metrics 时的占位约定。
 */
const metricText = (key: string, group: MarketGroup): string => {
  const value = group.stats?.[key]
  if (value == null) return t('market.insufficient')
  if (key === 'success_rate') return `${(value * 100).toFixed(1)}%`
  if (key === 'avg_latency_ms') return `${Math.round(value)} ms`
  return String(value)
}

/** 货架「使用」：跳转密钥创建流程并预选该分组。 */
const useGroup = (group: MarketGroup) => {
  void router.push({ path: '/keys', query: { group_id: String(group.id) } })
}

onMounted(load)
</script>
