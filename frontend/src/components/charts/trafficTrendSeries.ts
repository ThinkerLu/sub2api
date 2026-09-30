export type TrafficTrendField =
  | 'ingress_body_bytes'
  | 'upstream_request_body_bytes'
  | 'upstream_response_body_bytes'
  | 'egress_body_bytes'

export type TrafficTrendPairKey = 'request' | 'response'
export type TrafficTrendStroke = 'deep' | 'light'
export type TrafficTrendTheme = 'light' | 'dark'

export interface TrafficTrendDirection {
  field: TrafficTrendField
  label: string
  pair: TrafficTrendPairKey
  stroke: TrafficTrendStroke
}

export interface TrafficTrendPairSeries {
  key: TrafficTrendPairKey
  deep: { field: TrafficTrendField; label: string }
  light: { field: TrafficTrendField; label: string }
}

export interface TrafficTrendPairColors {
  deep: string
  light: string
}

export type TrafficTrendPalette = Record<TrafficTrendPairKey, TrafficTrendPairColors> & {
  total: string
}

// 每对指标里，深色实线是路径起点、浅色虚线是下一跳。两组值接近时虚线被实线盖住，
// 只剩一条深色实线；出现差异时虚线从实线后面露出，两跳的落差随之可见。
export const trafficTrendPairs: readonly TrafficTrendPairSeries[] = [
  {
    key: 'request',
    deep: { field: 'ingress_body_bytes', label: 'trafficIngress' },
    light: { field: 'upstream_request_body_bytes', label: 'trafficUpstreamRequest' }
  },
  {
    key: 'response',
    deep: { field: 'upstream_response_body_bytes', label: 'trafficUpstreamResponse' },
    light: { field: 'egress_body_bytes', label: 'trafficEgress' }
  }
]

export const trafficTrendDirections: readonly TrafficTrendDirection[] = trafficTrendPairs.flatMap((pair) => [
  { ...pair.deep, pair: pair.key, stroke: 'deep' as const },
  { ...pair.light, pair: pair.key, stroke: 'light' as const }
])

// 五条曲线共用同一套明度梯度：深色实线 < 总量 < 浅色虚线。请求对用蓝、响应对用橙，
// 同对只差明度与虚实，跨对靠色相分开，总量取中性灰并走右轴。
export const trafficTrendPalettes: Record<TrafficTrendTheme, TrafficTrendPalette> = {
  light: {
    request: { deep: '#1e3a8a', light: '#3b82f6' },
    response: { deep: '#9a3412', light: '#f97316' },
    total: '#64748b'
  },
  dark: {
    request: { deep: '#3b82f6', light: '#7dd3fc' },
    response: { deep: '#f97316', light: '#fdba74' },
    total: '#e2e8f0'
  }
}
