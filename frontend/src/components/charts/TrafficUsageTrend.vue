<template>
  <div class="card p-4">
    <h3 class="mb-4 text-sm font-semibold text-gray-900 dark:text-white">
      {{ t('admin.dashboard.trafficTrend') }}
    </h3>
    <div class="h-72 sm:h-80">
      <div v-if="loading" class="flex h-full items-center justify-center">
        <LoadingSpinner />
      </div>
      <Line v-else-if="trendData.length" :data="chartData" :options="lineOptions" />
      <div v-else class="flex h-full items-center justify-center text-sm text-gray-500 dark:text-gray-400">
        {{ t('admin.dashboard.noDataAvailable') }}
      </div>
    </div>
  </div>
</template>

<script setup lang="ts">
import { computed } from 'vue'
import { useI18n } from 'vue-i18n'
import { Line } from 'vue-chartjs'
import LoadingSpinner from '@/components/common/LoadingSpinner.vue'
import type { TrendDataPoint } from '@/types'
import { formatBytes } from '@/utils/formatters'

const { t } = useI18n()
const props = defineProps<{ trendData: TrendDataPoint[]; loading?: boolean }>()

const directions = [
  { field: 'ingress_body_bytes', label: 'trafficIngress', color: '#059669' },
  { field: 'upstream_request_body_bytes', label: 'trafficUpstreamRequest', color: '#d97706' },
  { field: 'upstream_response_body_bytes', label: 'trafficUpstreamResponse', color: '#0284c7' },
  { field: 'egress_body_bytes', label: 'trafficEgress', color: '#7c3aed' }
] as const

const totalBytes = (point: TrendDataPoint) =>
  directions.reduce((sum, direction) => sum + (point[direction.field] || 0), 0)

const axisMaximum = (max: number) => max > 0 ? max * 1.1 : 1
const directionMax = computed(() => axisMaximum(Math.max(0,
  ...props.trendData.flatMap((point) => directions.map(({ field }) => point[field] || 0))
)))
const totalMax = computed(() => axisMaximum(Math.max(0, ...props.trendData.map(totalBytes))))

const chartData = computed(() => ({
  labels: props.trendData.map((point) => point.date),
  datasets: [
    ...directions.map(({ field, label, color }) => ({
      label: t(`admin.dashboard.${label}`),
      data: props.trendData.map((point) => point[field] || 0),
      borderColor: color,
      backgroundColor: color,
      yAxisID: 'y',
      pointRadius: 1,
      tension: 0.3
    })),
    {
      label: t('admin.dashboard.trafficAll'),
      data: props.trendData.map(totalBytes),
      borderColor: '#e11d48',
      backgroundColor: '#e11d48',
      borderWidth: 2.5,
      yAxisID: 'yTotal',
      pointRadius: 1,
      tension: 0.3
    }
  ]
}))

const lineOptions = computed(() => {
  const dark = document.documentElement.classList.contains('dark')
  const text = dark ? '#e5e7eb' : '#374151'
  const grid = dark ? '#374151' : '#e5e7eb'
  return {
    responsive: true,
    maintainAspectRatio: false,
    interaction: { intersect: false, mode: 'index' as const },
    plugins: {
      legend: {
        position: 'top' as const,
        labels: { color: text, usePointStyle: true, pointStyle: 'circle' as const, font: { size: 11 } }
      },
      tooltip: { callbacks: { label: (context: { dataset: { label?: string }; raw: unknown }) =>
        `${context.dataset.label ?? ''}: ${formatBytes(Number(context.raw))}` } }
    },
    scales: {
      x: { grid: { color: grid }, ticks: { color: text } },
      y: {
        position: 'left' as const, min: 0, max: directionMax.value,
        grid: { color: grid }, ticks: { color: text, callback: (value: string | number) => formatBytes(Number(value)) }
      },
      yTotal: {
        position: 'right' as const, min: 0, max: totalMax.value,
        grid: { drawOnChartArea: false },
        ticks: { color: '#e11d48', callback: (value: string | number) => formatBytes(Number(value)) }
      }
    }
  }
})
</script>
