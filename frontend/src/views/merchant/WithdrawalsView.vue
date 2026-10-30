<template>
  <AppLayout>
    <div class="space-y-6">
      <header class="flex flex-wrap items-start justify-between gap-4">
        <div>
          <div class="flex items-center gap-2">
            <h1 class="text-2xl font-semibold text-gray-900 dark:text-white">
              {{ t('merchant.withdrawals.title') }}
            </h1>
            <span class="badge badge-primary">{{ t('merchant.dashboard.badge') }}</span>
          </div>
          <p class="mt-1 text-sm text-gray-500 dark:text-dark-400">
            {{ t('merchant.withdrawals.description') }}
          </p>
        </div>
        <RouterLink to="/merchant/dashboard" class="btn btn-secondary btn-sm">
          {{ t('merchant.withdrawals.backToOverview') }}
        </RouterLink>
      </header>

      <div v-if="loading" class="flex justify-center py-16">
        <div class="h-8 w-8 animate-spin rounded-full border-2 border-primary-500 border-t-transparent"></div>
      </div>

      <div v-else-if="loadFailed" class="card p-8 text-center">
        <p class="text-gray-600 dark:text-dark-300">{{ t('merchant.loadFailed') }}</p>
        <button class="btn btn-secondary mt-4" @click="load">{{ t('merchant.retry') }}</button>
      </div>

      <template v-else>
        <section class="grid gap-4 md:grid-cols-3">
          <article class="card p-5">
            <p class="text-sm text-gray-500 dark:text-dark-400">{{ t('merchant.withdrawals.stats.available') }}</p>
            <p class="mt-2 text-2xl font-semibold text-emerald-600 dark:text-emerald-400">
              {{ formatCurrency(summary?.available ?? 0) }}
            </p>
            <p class="mt-1 text-xs text-gray-400 dark:text-dark-500">
              {{ t('merchant.withdrawals.stats.availableHint') }}
            </p>
          </article>
          <article class="card p-5">
            <p class="text-sm text-gray-500 dark:text-dark-400">{{ t('merchant.withdrawals.stats.minimum') }}</p>
            <p class="mt-2 text-2xl font-semibold text-gray-900 dark:text-white">
              {{ formatCurrency(minimumAmount) }}
            </p>
            <p class="mt-1 text-xs text-gray-400 dark:text-dark-500">
              {{ t('merchant.withdrawals.stats.minimumHint') }}
            </p>
          </article>
          <article class="card p-5">
            <p class="text-sm text-gray-500 dark:text-dark-400">{{ t('merchant.withdrawals.stats.inFlight') }}</p>
            <p class="mt-2 text-2xl font-semibold text-gray-900 dark:text-white">
              {{ openWithdrawal ? formatCurrency(openWithdrawal.amount) : '-' }}
            </p>
            <p class="mt-1 text-xs text-gray-400 dark:text-dark-500">
              {{ openWithdrawal ? t(`merchant.withdrawals.status.${openWithdrawal.status}`) : t('merchant.withdrawals.stats.noInFlight') }}
            </p>
          </article>
        </section>

        <section class="card p-6">
          <div class="flex flex-wrap items-start justify-between gap-3">
            <div>
              <h2 class="text-base font-semibold text-gray-900 dark:text-white">
                {{ t('merchant.withdrawals.form.title') }}
              </h2>
              <p class="mt-1 text-sm text-gray-500 dark:text-dark-400">
                {{ t('merchant.withdrawals.form.description') }}
              </p>
            </div>
          </div>

          <p
            v-if="openWithdrawal"
            class="mt-4 rounded-xl border border-amber-200 bg-amber-50 p-4 text-sm text-amber-700 dark:border-amber-900/40 dark:bg-amber-900/20 dark:text-amber-300"
          >
            {{ t('merchant.withdrawals.form.openNotice') }}
          </p>

          <form class="mt-4 flex flex-col gap-3 sm:flex-row sm:items-end" @submit.prevent="submit">
            <label class="flex-1 text-sm font-medium text-gray-700 dark:text-gray-300">
              <span>{{ t('merchant.withdrawals.form.amount') }}</span>
              <input
                v-model.number="amount"
                class="input mt-1.5 w-full"
                type="number"
                inputmode="decimal"
                min="0.01"
                step="0.01"
                :placeholder="t('merchant.withdrawals.form.placeholder', { min: minimumAmount })"
                :disabled="requesting || !!openWithdrawal"
              />
            </label>
            <button
              type="submit"
              class="btn btn-primary sm:mb-0.5"
              :disabled="requesting || !canSubmit"
            >
              <span v-if="requesting">{{ t('merchant.withdrawals.form.submitting') }}</span>
              <span v-else>{{ t('merchant.withdrawals.form.submit') }}</span>
            </button>
          </form>
          <p class="mt-2 text-xs text-gray-400 dark:text-dark-500">
            {{ t('merchant.withdrawals.form.hint', { min: minimumAmount }) }}
          </p>
          <p v-if="formError" class="mt-3 text-sm text-red-600 dark:text-red-400">{{ formError }}</p>
          <p v-if="formSuccess" class="mt-3 text-sm text-emerald-600 dark:text-emerald-400">{{ formSuccess }}</p>
        </section>

        <section class="card p-6">
          <h2 class="text-base font-semibold text-gray-900 dark:text-white">
            {{ t('merchant.withdrawals.history.title') }}
          </h2>

          <div
            v-if="items.length === 0"
            class="mt-4 rounded-xl border border-dashed border-gray-300 p-6 text-center text-sm text-gray-500 dark:border-dark-700 dark:text-dark-400"
          >
            {{ t('merchant.withdrawals.history.empty') }}
          </div>
          <div v-else class="mt-4 overflow-x-auto">
            <table class="w-full min-w-[640px] text-left text-sm">
              <thead>
                <tr class="border-b border-gray-200 text-gray-500 dark:border-dark-700 dark:text-dark-400">
                  <th class="px-3 py-2 font-medium">{{ t('merchant.withdrawals.history.columns.id') }}</th>
                  <th class="px-3 py-2 text-right font-medium">{{ t('merchant.withdrawals.history.columns.amount') }}</th>
                  <th class="px-3 py-2 font-medium">{{ t('merchant.withdrawals.history.columns.status') }}</th>
                  <th class="px-3 py-2 font-medium">{{ t('merchant.withdrawals.history.columns.requestedAt') }}</th>
                  <th class="px-3 py-2 font-medium">{{ t('merchant.withdrawals.history.columns.paidAt') }}</th>
                </tr>
              </thead>
              <tbody>
                <tr
                  v-for="item in items"
                  :key="item.id"
                  class="border-b border-gray-100 last:border-b-0 dark:border-dark-800"
                >
                  <td class="px-3 py-3 font-mono text-xs text-gray-500 dark:text-dark-400">#{{ item.id }}</td>
                  <td class="px-3 py-3 text-right font-medium text-gray-900 dark:text-white">
                    {{ formatCurrency(item.amount) }}
                  </td>
                  <td class="px-3 py-3">
                    <span class="badge" :class="statusClass(item.status)">
                      {{ t(`merchant.withdrawals.status.${item.status}`) }}
                    </span>
                  </td>
                  <td class="px-3 py-3 text-gray-700 dark:text-gray-300">{{ formatDateTime(item.requested_at) || '-' }}</td>
                  <td class="px-3 py-3 text-gray-700 dark:text-gray-300">{{ formatDateTime(item.paid_at) || '-' }}</td>
                </tr>
              </tbody>
            </table>
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
import {
  merchantAPI,
  type MerchantWithdrawal,
  type MerchantWithdrawalStatus,
  type MerchantWithdrawals,
} from '@/api/merchant'
import { extractApiErrorMessage } from '@/utils/apiError'
import { formatCurrency, formatDateTime } from '@/utils/format'

const defaultMinimumAmount = 50
const { t } = useI18n()

const summary = ref<MerchantWithdrawals | null>(null)
const loading = ref(false)
const loadFailed = ref(false)
const requesting = ref(false)
const amount = ref<number | null>(null)
const formError = ref('')
const formSuccess = ref('')

const items = computed<MerchantWithdrawal[]>(() => summary.value?.items ?? [])
const openWithdrawal = computed(() =>
  items.value.find((item) => item.status === 'requested' || item.status === 'approved'),
)
const available = computed(() => summary.value?.available ?? 0)
const minimumAmount = computed(() => summary.value?.minimum ?? defaultMinimumAmount)
const canSubmit = computed(() => {
  const value = amount.value
  if (value === null || !Number.isFinite(value)) return false
  return value >= minimumAmount.value && value <= available.value
})

const errorMessages = computed<Record<string, string>>(() => ({
  WITHDRAWAL_BELOW_MINIMUM: t('merchant.withdrawals.errors.belowMinimum', { min: minimumAmount.value }),
  WITHDRAWAL_EXCEEDS_AVAILABLE: t('merchant.withdrawals.errors.exceedsAvailable'),
  WITHDRAWAL_AMOUNT_INVALID: t('merchant.withdrawals.errors.invalidAmount'),
  WITHDRAWAL_IN_FLIGHT: t('merchant.withdrawals.errors.inFlight'),
  WITHDRAWAL_ALREADY_PAID: t('merchant.withdrawals.errors.alreadyPaid'),
}))

function statusClass(status: MerchantWithdrawalStatus): string {
  switch (status) {
    case 'paid':
      return 'badge-success'
    case 'approved':
      return 'badge-primary'
    case 'rejected':
      return 'badge-danger'
    default:
      return 'badge-warning'
  }
}

async function load(): Promise<void> {
  loading.value = true
  loadFailed.value = false
  try {
    summary.value = await merchantAPI.getMerchantWithdrawals()
  } catch (error) {
    console.error('Failed to load merchant withdrawals:', error)
    loadFailed.value = true
  } finally {
    loading.value = false
  }
}

async function submit(): Promise<void> {
  if (!canSubmit.value || amount.value === null) return
  requesting.value = true
  formError.value = ''
  formSuccess.value = ''
  try {
    await merchantAPI.requestMerchantWithdrawal(amount.value)
    amount.value = null
    formSuccess.value = t('merchant.withdrawals.form.success')
    await load()
  } catch (error) {
    formError.value = extractApiErrorMessage(
      error,
      t('merchant.withdrawals.errors.failed'),
      errorMessages.value,
    )
  } finally {
    requesting.value = false
  }
}

onMounted(load)
</script>
