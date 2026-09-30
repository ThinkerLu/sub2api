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
import { computed, onBeforeUnmount, onMounted, ref } from 'vue'
import { useI18n } from 'vue-i18n'
import { Line } from 'vue-chartjs'
import LoadingSpinner from '@/components/common/LoadingSpinner.vue'
import type { TrendDataPoint } from '@/types'
import { formatBytes } from '@/utils/formatters'
import { trafficTrendDirections, trafficTrendPairs, trafficTrendPalettes } from './trafficTrendSeries'

const { t } = useI18n()
const props = defineProps<{ trendData: TrendDataPoint[]; loading?: boolean }>()

// 主题切换只改 <html> 的 class，用观察器让曲线颜色跟着重绘。
const isDarkMode = ref(typeof document !== 'undefined' && document.documentElement.classList.contains('dark'))
let themeObserver: MutationObserver | null = null

onMounted(() => {
  themeObserver = new MutationObserver(() => {
    isDarkMode.value = document.documentElement.classList.contains('dark')
  })
  themeObserver.observe(document.documentElement, { attributes: true, attributeFilter: ['class'] })
})

onBeforeUnmount(() => themeObserver?.disconnect())

const palette = computed(() => trafficTrendPalettes[isDarkMode.value ? 'dark' : 'light'])

const totalBytes = (point: TrendDataPoint) =>
  trafficTrendDirections.reduce((sum, direction) => sum + (point[direction.field] || 0), 0)

const axisMaximum = (max: number) => max > 0 ? max * 1.1 : 1
const directionMax = computed(() => axisMaximum(Math.max(0,
  ...props.trendData.flatMap((point) => trafficTrendDirections.map(({ field }) => point[field] || 0))
)))
const totalMax = computed(() => axisMaximum(Math.max(0, ...props.trendData.map(totalBytes))))

const chartData = computed(() => {
  const colors = palette.value
  return {
    labels: props.trendData.map((point) => point.date),
    datasets: [
      ...trafficTrendDirections.map((direction) => {
        const pairColors = colors[direction.pair]
        const color = pairColors[direction.stroke]
        const dashed = direction.stroke === 'light'
        return {
          label: t(`admin.dashboard.${direction.label}`),
          data: props.trendData.map((point) => point[direction.field] || 0),
          borderColor: color,
          backgroundColor: color,
          borderWidth: dashed ? 1.5 : 2.2,
          borderDash: dashed ? [5, 4] : undefined,
          fill: false as const,
          pointRadius: 1,
          pointHoverRadius: 4,
          pointBackgroundColor: color,
          pointBorderColor: color,
          yAxisID: 'y',
          tension: 0.3
        }
      }),
      {
        label: t('admin.dashboard.trafficAll'),
        data: props.trendData.map(totalBytes),
        borderColor: colors.total,
        backgroundColor: colors.total,
        borderWidth: 2.5,
        fill: false as const,
        yAxisID: 'yTotal',
        pointRadius: 1,
        pointHoverRadius: 4,
        tension: 0.3
      }
    ]
  }
})

// 悬停时只列出真正出现差值的指标对，便于确认是哪一对不匹配及差多少。
const pairDeltaLines = (dataIndex?: number) => {
  const point = dataIndex === undefined ? undefined : props.trendData[dataIndex]
  if (!point) return []
  return trafficTrendPairs.flatMap((pair) => {
    const first = point[pair.deep.field] || 0
    const second = point[pair.light.field] || 0
    const delta = Math.abs(first - second)
    if (!delta) return []
    const percent = (delta / Math.max(first, second, 1)) * 100
    return [t('admin.dashboard.trafficPairDelta', {
      first: t(`admin.dashboard.${pair.deep.label}`),
      second: t(`admin.dashboard.${pair.light.label}`),
      delta: formatBytes(delta),
      percent: percent.toFixed(1)
    })]
  })
}

const lineOptions = computed(() => {
  const colors = palette.value
  const dark = isDarkMode.value
  const text = dark ? '#e5e7eb' : '#374151'
  const grid = dark ? '#374151' : '#e5e7eb'
  return {
    responsive: true,
    maintainAspectRatio: false,
    interaction: { intersect: false, mode: 'index' as const },
    plugins: {
      legend: {
        position: 'top' as const,
        labels: { color: text, usePointStyle: true, pointStyle: 'line' as const, font: { size: 11 } }
      },
      tooltip: {
        callbacks: {
          label: (context: { dataset: { label?: string }; raw: unknown }) =>
            `${context.dataset.label ?? ''}: ${formatBytes(Number(context.raw))}`,
          footer: (items: Array<{ dataIndex: number }>) => pairDeltaLines(items[0]?.dataIndex)
        }
      }
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
        ticks: { color: colors.total, callback: (value: string | number) => formatBytes(Number(value)) }
      }
    }
  }
})
</script>
