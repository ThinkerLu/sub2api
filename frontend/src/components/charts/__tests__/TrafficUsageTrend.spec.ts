import { afterEach, describe, expect, it, vi } from 'vitest'
import { mount, type VueWrapper } from '@vue/test-utils'
import { nextTick } from 'vue'
import TrafficUsageTrend from '../TrafficUsageTrend.vue'
import { trafficTrendDirections, trafficTrendPalettes } from '../trafficTrendSeries'

vi.mock('vue-i18n', () => ({ useI18n: () => ({ t: (key: string) => key }) }))
vi.mock('vue-chartjs', () => ({
  Line: { name: 'Line', props: ['data', 'options'], template: '<div class="chart" />' }
}))

type ChartDataset = {
  borderColor: string
  backgroundColor: string
  borderWidth: number
  borderDash?: number[]
  fill: false
  yAxisID: string
  data: number[]
}

const sample = (overrides: Record<string, unknown>[] = []) => [
  { date: '2026-09-25 10:00', ingress_body_bytes: 100, upstream_request_body_bytes: 200, upstream_response_body_bytes: 300, egress_body_bytes: 400, ...overrides[0] },
  { date: '2026-09-25 11:00', ingress_body_bytes: 500, upstream_request_body_bytes: 600, upstream_response_body_bytes: 700, egress_body_bytes: 800, ...overrides[1] }
]

const mountTrend = (trendData: Record<string, unknown>[] = sample()) => mount(TrafficUsageTrend, {
  props: { trendData: trendData as never },
  global: { stubs: { LoadingSpinner: true } }
})

const lineOf = (wrapper: VueWrapper) => wrapper.findComponent({ name: 'Line' })
const datasetsOf = (wrapper: VueWrapper) => lineOf(wrapper).props('data').datasets as ChartDataset[]

const toRgb = (hex: string) => hex.replace('#', '').match(/.{2}/g)?.map((part) => parseInt(part, 16)) ?? []

const relativeLuminance = (hex: string) => {
  const [r, g, b] = toRgb(hex).map((channel) => {
    const value = channel / 255
    return value <= 0.03928 ? value / 12.92 : ((value + 0.055) / 1.055) ** 2.4
  })
  return 0.2126 * r + 0.7152 * g + 0.0722 * b
}

const hue = (hex: string) => {
  const [r, g, b] = toRgb(hex).map((channel) => channel / 255)
  const max = Math.max(r, g, b)
  const delta = max - Math.min(r, g, b)
  if (!delta) return 0
  const raw = max === r ? ((g - b) / delta) % 6 : max === g ? (b - r) / delta + 2 : (r - g) / delta + 4
  return (raw * 60 + 360) % 360
}

const hueDistance = (a: number, b: number) => {
  const distance = Math.abs(a - b) % 360
  return distance > 180 ? 360 - distance : distance
}

afterEach(() => {
  document.documentElement.classList.remove('dark')
})

describe('TrafficUsageTrend', () => {
  it('uses four directional series on the left and their sum on the right', () => {
    const wrapper = mountTrend()
    const datasets = datasetsOf(wrapper)
    expect(datasets).toHaveLength(5)
    expect(datasets.map((data) => data.yAxisID)).toEqual(['y', 'y', 'y', 'y', 'yTotal'])
    expect(datasets[4].data).toEqual([1000, 2600])
    expect(lineOf(wrapper).props('options').scales.y.max).toBeCloseTo(880)
    expect(lineOf(wrapper).props('options').scales.yTotal.max).toBeCloseTo(2860)
  })

  it('shows an empty state when the selected range has no usage', () => {
    const wrapper = mountTrend([])
    expect(wrapper.find('.chart').exists()).toBe(false)
    expect(wrapper.text()).toContain('admin.dashboard.noDataAvailable')
  })

  it('keeps each pair on one hue and separates the two members by lightness', () => {
    for (const palette of Object.values(trafficTrendPalettes)) {
      const requestHue = hue(palette.request.light)
      const responseHue = hue(palette.response.light)
      expect(hueDistance(requestHue, hue(palette.request.deep))).toBeLessThanOrEqual(25)
      expect(hueDistance(responseHue, hue(palette.response.deep))).toBeLessThanOrEqual(25)
      expect(hueDistance(requestHue, responseHue)).toBeGreaterThanOrEqual(90)
      expect(relativeLuminance(palette.request.light) - relativeLuminance(palette.request.deep)).toBeGreaterThan(0.15)
      expect(relativeLuminance(palette.response.light) - relativeLuminance(palette.response.deep)).toBeGreaterThan(0.15)
      const fiveColors = [palette.request.deep, palette.request.light, palette.response.deep, palette.response.light, palette.total]
      expect(new Set(fiveColors).size).toBe(fiveColors.length)
      // 总量既不与深色实线重合，也不与浅色虚线抢同一级明度。
      expect(relativeLuminance(palette.total)).toBeGreaterThan(relativeLuminance(palette.request.deep) + 0.05)
      expect(relativeLuminance(palette.total)).toBeGreaterThan(relativeLuminance(palette.response.deep) + 0.05)
      expect(Math.abs(relativeLuminance(palette.total) - relativeLuminance(palette.request.light))).toBeGreaterThan(0.05)
      expect(Math.abs(relativeLuminance(palette.total) - relativeLuminance(palette.response.light))).toBeGreaterThan(0.05)
    }
  })

  it('draws the lighter series dashed and leaves the gap between each pair unfilled', () => {
    const datasets = datasetsOf(mountTrend())
    expect(datasets[0].borderDash).toBeUndefined()
    expect(datasets[2].borderDash).toBeUndefined()
    expect(datasets[1].borderDash).toEqual([5, 4])
    expect(datasets[3].borderDash).toEqual([5, 4])
    expect(datasets.map((dataset) => dataset.fill)).toEqual([false, false, false, false, false])
    expect(datasets[1].borderWidth).toBeLessThan(datasets[0].borderWidth)
    expect(datasets[3].borderWidth).toBeLessThan(datasets[2].borderWidth)
    // Chart.js 后绘制的数据集在上层：实线要排在同对虚线前面才会盖住虚线。
    expect(trafficTrendDirections.map((direction) => direction.stroke)).toEqual(['deep', 'light', 'deep', 'light'])
  })

  it('repaints with the dark palette once the html class changes', async () => {
    const wrapper = mountTrend()
    expect(datasetsOf(wrapper)[0].borderColor).toBe(trafficTrendPalettes.light.request.deep)
    expect(lineOf(wrapper).props('options').scales.yTotal.ticks.color).toBe(trafficTrendPalettes.light.total)

    document.documentElement.classList.add('dark')
    await new Promise((resolve) => setTimeout(resolve, 0))
    await nextTick()

    expect(datasetsOf(wrapper)[0].borderColor).toBe(trafficTrendPalettes.dark.request.deep)
    expect(lineOf(wrapper).props('options').scales.yTotal.ticks.color).toBe(trafficTrendPalettes.dark.total)
  })

  it('lists only the mismatched pair in the tooltip footer', () => {
    const wrapper = mountTrend(sample([
      { ingress_body_bytes: 100, upstream_request_body_bytes: 100 },
      { upstream_response_body_bytes: 800, egress_body_bytes: 800 }
    ]))
    const footer = lineOf(wrapper).props('options').plugins.tooltip.callbacks.footer
    expect(footer([])).toEqual([])
    expect(footer([{ dataIndex: 0 }])).toEqual(['admin.dashboard.trafficPairDelta'])
    expect(footer([{ dataIndex: 1 }])).toEqual(['admin.dashboard.trafficPairDelta'])
  })
})
