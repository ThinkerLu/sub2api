import { describe, expect, it } from 'vitest'
import { normalizeOpenAIWSMode, resolveOpenAIWSModeFromExtra } from '../openaiWsMode'
import { resolveUsageRequestType, usageTransportQuery } from '../usageRequestType'

describe('adaptive WS configuration and usage transport', () => {
  it('round trips adaptive for both credential types', () => {
    expect(normalizeOpenAIWSMode('adaptive')).toBe('adaptive')
    for (const type of ['oauth', 'apikey']) {
      const modeKey = `openai_${type}_responses_websockets_v2_mode`
      expect(resolveOpenAIWSModeFromExtra({ [modeKey]: 'adaptive' }, { modeKey, enabledKey: 'enabled' })).toBe('adaptive')
    }
  })
  it('uses actual per-request upstream transport without changing the stored request type', () => {
    expect(resolveUsageRequestType({ request_type: 'ws_v2', upstream_transport: 1 })).toBe('ws_http')
    expect(resolveUsageRequestType({ request_type: 'ws_v2', upstream_transport: 2 })).toBe('ws_v2')
    expect(resolveUsageRequestType({ request_type: 'ws_v2', upstream_transport: 0 })).toBe('ws_v2')
    expect(resolveUsageRequestType({ request_type: 'cyber', upstream_transport: 1 })).toBe('cyber')
    expect(resolveUsageRequestType({ request_type: 'stream', upstream_transport: 1 })).toBe('stream')
  })
  it('encodes a bridge filter as legacy ws_v2 plus the additive transport field', () => {
    const query = { request_type: 'ws_http', page: 2 }
    expect(usageTransportQuery(query)).toEqual({ request_type: 'ws_v2', upstream_transport: 1, page: 2 })
    expect(query.request_type).toBe('ws_http')
    expect(usageTransportQuery({ request_type: 'ws_v2' })).toEqual({ request_type: 'ws_v2' })
  })
  it('derives all four actual transport combinations and preserves business types', () => {
    expect(resolveUsageRequestType({ ingress_transport: 1, upstream_transport: 2, request_type: 'ws_v2' })).toBe('http_ws')
    expect(resolveUsageRequestType({ ingress_transport: 2, upstream_transport: 1 })).toBe('ws_http')
    expect(resolveUsageRequestType({ ingress_transport: 2, upstream_transport: 2 })).toBe('ws_v2')
    expect(resolveUsageRequestType({ ingress_transport: 1, upstream_transport: 1, stream: true, request_type: 'ws_v2' })).toBe('stream')
    expect(resolveUsageRequestType({ ingress_transport: 1, upstream_transport: 1, stream: false })).toBe('sync')
    expect(resolveUsageRequestType({ ingress_transport: 0, upstream_transport: 2, request_type: 'ws_v2' })).toBe('ws_v2')
    for (const request_type of ['cyber', 'live'] as const) {
      expect(resolveUsageRequestType({ request_type, ingress_transport: 1, upstream_transport: 2 })).toBe(request_type)
    }
  })
  it('filters HTTP to WS using both transport columns without a legacy request enum', () => {
    const params = { request_type: 'http_ws', stream: true, page: 1 }
    expect(usageTransportQuery(params)).toEqual({ request_type: undefined, stream: undefined, ingress_transport: 1, upstream_transport: 2, page: 1 })
    expect(params.request_type).toBe('http_ws')
  })
})
