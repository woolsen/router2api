<template>
  <AppLayout>
    <div class="mx-auto max-w-3xl space-y-6">
      <header>
        <h1 class="text-2xl font-semibold text-gray-900 dark:text-white">
          {{ t('merchant.apply.title') }}
        </h1>
        <p class="mt-1 text-sm text-gray-500 dark:text-dark-400">
          {{ t('merchant.apply.description') }}
        </p>
      </header>

      <div v-if="authStore.isMerchant" class="card p-8 text-center">
        <p class="text-gray-700 dark:text-dark-200">
          {{ t('merchant.apply.alreadyMerchant') }}
        </p>
        <router-link to="/merchant/dashboard" class="btn btn-primary mt-4 inline-flex">
          {{ t('merchant.apply.goDashboard') }}
        </router-link>
      </div>

      <form v-else class="space-y-6" @submit.prevent="submit">
        <section class="card space-y-4 p-6">
          <h2 class="font-semibold text-gray-900 dark:text-white">
            {{ t('merchant.apply.upstreamTitle') }}
          </h2>

          <div class="grid gap-4 sm:grid-cols-2">
            <label class="block">
              <span class="input-label">{{ t('merchant.apply.merchantName') }}</span>
              <input
                v-model.trim="form.merchantName"
                class="input mt-1 w-full"
                data-test="merchant-name"
                required
                maxlength="100"
                :placeholder="t('merchant.apply.merchantNamePlaceholder')"
              />
            </label>

            <label class="block">
              <span class="input-label">{{ t('merchant.apply.platform') }}</span>
              <select v-model="form.platform" class="input mt-1 w-full" data-test="merchant-platform">
                <option value="openai">OpenAI</option>
                <option value="anthropic">Anthropic</option>
              </select>
            </label>
          </div>

          <label class="block">
            <span class="input-label">{{ t('merchant.apply.baseUrl') }}</span>
            <input
              v-model.trim="form.baseUrl"
              class="input mt-1 w-full font-mono"
              data-test="merchant-base-url"
              required
              type="url"
              :placeholder="t('merchant.apply.baseUrlPlaceholder')"
            />
            <span class="input-hint">{{ t('merchant.apply.baseUrlHint') }}</span>
          </label>

          <label class="block">
            <span class="input-label">{{ t('merchant.apply.apiKey') }}</span>
            <input
              v-model.trim="form.apiKey"
              class="input mt-1 w-full font-mono"
              data-test="merchant-api-key"
              required
              type="password"
              autocomplete="off"
              :placeholder="t('merchant.apply.apiKeyPlaceholder')"
            />
            <span class="input-hint">{{ t('merchant.apply.apiKeyHint') }}</span>
          </label>

          <label class="block sm:w-1/2">
            <span class="input-label">{{ t('merchant.apply.rate') }}</span>
            <input
              v-model.number="form.rateMultiplier"
              class="input mt-1 w-full"
              data-test="merchant-rate"
              type="number"
              min="0.01"
              step="0.01"
              required
            />
            <span class="input-hint">{{ t('merchant.apply.rateHint') }}</span>
          </label>
        </section>

        <section class="card space-y-4 p-6">
          <div class="flex flex-wrap items-start justify-between gap-3">
            <div>
              <h2 class="font-semibold text-gray-900 dark:text-white">
                {{ t('merchant.apply.probeTitle') }}
              </h2>
              <p class="mt-1 text-xs text-gray-400 dark:text-dark-500">
                {{ t('merchant.apply.probeHint') }}
              </p>
            </div>
            <button
              type="button"
              class="btn btn-secondary"
              data-test="merchant-probe"
              :disabled="!canProbe"
              @click="probe"
            >
              {{ probing ? t('merchant.apply.probing') : t('merchant.apply.probe') }}
            </button>
          </div>

          <p
            v-if="probeError"
            class="rounded-lg bg-red-50 px-3 py-2 text-sm text-red-700 dark:bg-red-900/20 dark:text-red-300"
            data-test="merchant-probe-error"
          >
            {{ probeError }}
          </p>

          <template v-if="probeResult">
            <div class="grid gap-3 sm:grid-cols-3">
              <div class="rounded-lg bg-gray-50 p-3 text-center dark:bg-dark-700/40">
                <p class="text-[11px] text-gray-500 dark:text-dark-400">
                  {{ t('merchant.apply.returnsUsage') }}
                </p>
                <p class="mt-1 text-sm font-medium text-emerald-600 dark:text-emerald-400">
                  {{ t('merchant.apply.supported') }}
                </p>
              </div>
              <div class="rounded-lg bg-gray-50 p-3 text-center dark:bg-dark-700/40">
                <p class="text-[11px] text-gray-500 dark:text-dark-400">
                  {{ t('merchant.apply.supportsStream') }}
                </p>
                <p class="mt-1 text-sm font-medium text-emerald-600 dark:text-emerald-400">
                  {{ t('merchant.apply.supported') }}
                </p>
              </div>
              <div class="rounded-lg bg-gray-50 p-3 text-center dark:bg-dark-700/40">
                <p class="text-[11px] text-gray-500 dark:text-dark-400">
                  {{ t('merchant.apply.latency') }}
                </p>
                <p class="mt-1 text-sm font-medium text-gray-900 dark:text-white">
                  {{ probeResult.latency_ms }} ms
                </p>
              </div>
            </div>

            <div>
              <div class="flex items-center justify-between">
                <p class="text-sm font-medium text-gray-700 dark:text-dark-200">
                  {{ t('merchant.apply.modelsTitle', { count: probeResult.models.length }) }}
                </p>
                <button
                  type="button"
                  class="text-xs text-primary-600 hover:underline dark:text-primary-400"
                  @click="toggleAllModels"
                >
                  {{ allModelsSelected ? t('merchant.apply.clearAll') : t('merchant.apply.selectAll') }}
                </button>
              </div>
              <div
                class="mt-2 grid max-h-56 gap-1.5 overflow-y-auto rounded-lg border border-gray-100 p-3 sm:grid-cols-2 dark:border-dark-700"
              >
                <label
                  v-for="model in probeResult.models"
                  :key="model"
                  class="flex items-center gap-2 rounded px-2 py-1 text-xs text-gray-600 hover:bg-gray-50 dark:text-dark-300 dark:hover:bg-dark-700/40"
                >
                  <input v-model="selectedModels" type="checkbox" :value="model" />
                  <span class="truncate font-mono">{{ model }}</span>
                </label>
              </div>
              <p v-if="selectedModels.length === 0" class="mt-1 text-xs text-amber-600 dark:text-amber-400">
                {{ t('merchant.apply.selectAtLeastOne') }}
              </p>
            </div>
          </template>
        </section>

        <div class="flex flex-wrap items-center justify-end gap-3">
          <p
            v-if="submitError"
            class="mr-auto text-sm text-red-600 dark:text-red-400"
            data-test="merchant-submit-error"
          >
            {{ submitError }}
          </p>
          <button
            type="submit"
            class="btn btn-primary"
            data-test="merchant-submit"
            :disabled="!canSubmit"
          >
            {{ submitting ? t('merchant.apply.submitting') : t('merchant.apply.submit') }}
          </button>
        </div>
      </form>
    </div>
  </AppLayout>
</template>

<script setup lang="ts">
import { computed, reactive, ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import { useRouter } from 'vue-router'
import AppLayout from '@/components/layout/AppLayout.vue'
import {
  merchantAPI,
  type MerchantApplyPlatform,
  type MerchantProbeResult,
} from '@/api/merchant'
import { useAuthStore } from '@/stores/auth'

const { t } = useI18n()
const router = useRouter()
const authStore = useAuthStore()

const form = reactive({
  merchantName: '',
  platform: 'openai' as MerchantApplyPlatform,
  baseUrl: '',
  apiKey: '',
  rateMultiplier: 1,
})

const probing = ref(false)
const submitting = ref(false)
const probeResult = ref<MerchantProbeResult | null>(null)
const probeError = ref('')
const submitError = ref('')
const selectedModels = ref<string[]>([])

const probeCodes = ['insecure_url', 'models_empty', 'upstream_returns_no_usage', 'stream_unsupported'] as const

const allModelsSelected = computed(() => {
  const models = probeResult.value?.models ?? []
  return models.length > 0 && selectedModels.value.length === models.length
})

const canSubmit = computed(
  () =>
    !!probeResult.value &&
    selectedModels.value.length > 0 &&
    !probing.value &&
    !submitting.value,
)

const canProbe = computed(() => !!form.baseUrl && !!form.apiKey && !probing.value && !submitting.value)

function errorMessage(error: unknown): string {
  const message = (error as { message?: string })?.message ?? ''
  if ((probeCodes as readonly string[]).includes(message)) {
    return t(`merchant.apply.errors.${message}`)
  }
  return message || t('merchant.apply.errors.fallback')
}

function toggleAllModels(): void {
  selectedModels.value = allModelsSelected.value ? [] : [...(probeResult.value?.models ?? [])]
}

async function probe(): Promise<void> {
  probing.value = true
  probeError.value = ''
  submitError.value = ''
  probeResult.value = null
  selectedModels.value = []
  try {
    const result = await merchantAPI.probeMerchantUpstream({
      base_url: form.baseUrl,
      api_key: form.apiKey,
      platform: form.platform,
    })
    probeResult.value = result
    selectedModels.value = [...result.models]
  } catch (error) {
    probeError.value = errorMessage(error)
  } finally {
    probing.value = false
  }
}

async function submit(): Promise<void> {
  submitting.value = true
  submitError.value = ''
  try {
    await merchantAPI.submitMerchantApply({
      merchant_name: form.merchantName,
      base_url: form.baseUrl,
      api_key: form.apiKey,
      platform: form.platform,
      rate_multiplier: form.rateMultiplier,
      models: selectedModels.value,
    })
    await authStore.refreshUser().catch((error) => {
      console.error('Failed to refresh user after merchant apply:', error)
    })
    await router.push('/merchant/dashboard')
  } catch (error) {
    submitError.value = errorMessage(error)
  } finally {
    submitting.value = false
  }
}

// 上游地址、key 或协议变更后，旧探测结论不再可信，必须重新探测。
watch(
  () => [form.baseUrl, form.apiKey, form.platform],
  () => {
    probeResult.value = null
    selectedModels.value = []
    probeError.value = ''
  },
)
</script>
