// examd 文件 API 的浏览器端封装。

export interface FsEntry {
  name: string
  path: string
  type: 'file' | 'dir'
  size: number
  mtime: string
  etag?: string
}

export interface ReadResult {
  data: Uint8Array
  etag: string
  mtime: string
}

export class ApiError extends Error {
  constructor(
    readonly status: number,
    readonly code: string,
    message: string,
    readonly currentEtag?: string
  ) {
    super(message)
    this.name = 'ApiError'
  }
}

async function request(input: string, init?: RequestInit): Promise<Response> {
  let res: Response
  try {
    res = await fetch(input, { credentials: 'same-origin', cache: 'no-store', ...init })
  } catch (err) {
    throw new ApiError(0, 'network', err instanceof Error ? err.message : 'network error')
  }
  if (!res.ok) {
    let payload: Record<string, unknown> = {}
    try {
      payload = (await res.json()) as Record<string, unknown>
    } catch {
      // 非 JSON 错误体，保留状态码即可。
    }
    throw new ApiError(
      res.status,
      typeof payload.code === 'string' ? payload.code : 'http_error',
      typeof payload.error === 'string' ? payload.error : res.statusText,
      typeof payload.current_etag === 'string' ? payload.current_etag : undefined
    )
  }
  return res
}

export function encodePath(p: string): string {
  return encodeURIComponent(p.startsWith('/') ? p : '/' + p)
}

export const api = {
  async session(): Promise<
    { authenticated: false } | { authenticated: true; student_no: string; name: string }
  > {
    const res = await request('/api/session')
    return (await res.json()) as
      | { authenticated: false }
      | { authenticated: true; student_no: string; name: string }
  },

  async me(): Promise<{ student_no: string; name: string; workspace: string }> {
    const res = await request('/api/me')
    return (await res.json()) as { student_no: string; name: string; workspace: string }
  },

  async login(studentNo: string, token: string): Promise<void> {
    await request('/api/login', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ student_no: studentNo, token })
    })
  },

  async logout(): Promise<void> {
    await request('/api/logout', { method: 'POST' })
  },

  async list(path: string): Promise<FsEntry[]> {
    const res = await request(`/api/fs/list?path=${encodePath(path)}`)
    const body = (await res.json()) as { entries: FsEntry[] }
    return body.entries ?? []
  },

  async stat(path: string): Promise<FsEntry> {
    const res = await request(`/api/fs/stat?path=${encodePath(path)}`)
    return (await res.json()) as FsEntry
  },

  async read(path: string): Promise<ReadResult> {
    const res = await request(`/api/fs/read?path=${encodePath(path)}`)
    const buf = new Uint8Array(await res.arrayBuffer())
    return {
      data: buf,
      etag: res.headers.get('ETag') ?? '',
      mtime: res.headers.get('X-File-Mtime') ?? new Date().toISOString()
    }
  },

  async write(path: string, data: Uint8Array, ifMatch?: string): Promise<FsEntry> {
    const headers: Record<string, string> = { 'Content-Type': 'application/octet-stream' }
    if (ifMatch) headers['If-Match'] = ifMatch
    const res = await request(`/api/fs/write?path=${encodePath(path)}`, {
      method: 'PUT',
      headers,
      body: data as unknown as BodyInit
    })
    return (await res.json()) as FsEntry
  },

  async mkdir(path: string): Promise<void> {
    await request(`/api/fs/mkdir?path=${encodePath(path)}`, { method: 'POST' })
  },

  async rename(from: string, to: string, overwrite: boolean): Promise<void> {
    await request('/api/fs/rename', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ from, to, overwrite })
    })
  },

  async remove(path: string, recursive: boolean): Promise<void> {
    await request(`/api/fs/delete?path=${encodePath(path)}&recursive=${recursive ? 1 : 0}`, {
      method: 'DELETE'
    })
  }
}
