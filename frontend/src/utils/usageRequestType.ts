import type { UsageRequestType } from '@/types'

export interface UsageRequestTypeLike {
  request_type?: string | null
  stream?: boolean | null
  openai_ws_mode?: boolean | null
  upstream_transport?: number | null
}

const VALID_REQUEST_TYPES = new Set<UsageRequestType>(['unknown', 'sync', 'stream', 'ws_v2', 'ws_http', 'cyber', 'live'])

export const isUsageRequestType = (value: unknown): value is UsageRequestType => {
  return typeof value === 'string' && VALID_REQUEST_TYPES.has(value as UsageRequestType)
}

export const resolveUsageRequestType = (value: UsageRequestTypeLike): UsageRequestType => {
  if (value.upstream_transport === 1 && (value.request_type === 'ws_v2' || (!value.request_type && value.openai_ws_mode))) return 'ws_http'
  if (isUsageRequestType(value.request_type)) {
    return value.request_type
  }
  if (value.openai_ws_mode) {
    return 'ws_v2'
  }
  return value.stream ? 'stream' : 'sync'
}

export const requestTypeToLegacyStream = (requestType?: UsageRequestType | null): boolean | null | undefined => {
  // cyber 与 stream 正交（cyber 可发生在 stream 或非 stream 请求），不映射到 legacy stream 维度。
  if (!requestType || requestType === 'unknown' || requestType === 'cyber' || requestType === 'live') {
    return null
  }
  if (requestType === 'sync') {
    return false
  }
  return true
}

// UI subtype is encoded as a separate additive filter, never a new DB enum.
export const usageTransportQuery = <T extends object>(params: T): T & { upstream_transport?: number } => {
  if ((params as { request_type?: unknown }).request_type === 'ws_http') return { ...params, request_type: 'ws_v2', upstream_transport: 1 }
  return params
}
