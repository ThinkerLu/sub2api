import { describe, expect, it, vi } from 'vitest'
import { mount } from '@vue/test-utils'
import TrafficUsageTrend from '../TrafficUsageTrend.vue'

vi.mock('vue-i18n', () => ({ useI18n: () => ({ t: (key: string) => key }) }))
vi.mock('vue-chartjs', () => ({
  Line: { name: 'Line', props: ['data', 'options'], template: '<div class="chart" />' }
}))

describe('TrafficUsageTrend', () => {
  it('uses four directional series on the left and their sum on the right', () => {
    const wrapper = mount(TrafficUsageTrend, {
      props: {
        trendData: [
          { date: '2026-09-25 10:00', ingress_body_bytes: 100, upstream_request_body_bytes: 200, upstream_response_body_bytes: 300, egress_body_bytes: 400 },
          { date: '2026-09-25 11:00', ingress_body_bytes: 500, upstream_request_body_bytes: 600, upstream_response_body_bytes: 700, egress_body_bytes: 800 }
        ] as any
      },
      global: { stubs: { LoadingSpinner: true } }
    })
    const line = wrapper.findComponent({ name: 'Line' })
    const datasets = line.props('data').datasets
    expect(datasets).toHaveLength(5)
    expect(datasets.map((data: { yAxisID: string }) => data.yAxisID)).toEqual(['y', 'y', 'y', 'y', 'yTotal'])
    expect(datasets[4].data).toEqual([1000, 2600])
    expect(line.props('options').scales.y.max).toBeCloseTo(880)
    expect(line.props('options').scales.yTotal.max).toBeCloseTo(2860)
  })

  it('shows an empty state when the selected range has no usage', () => {
    const wrapper = mount(TrafficUsageTrend, { props: { trendData: [] }, global: { stubs: { LoadingSpinner: true } } })
    expect(wrapper.find('.chart').exists()).toBe(false)
    expect(wrapper.text()).toContain('admin.dashboard.noDataAvailable')
  })
})
