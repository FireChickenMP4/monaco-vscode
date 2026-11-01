// RemoteFileSystemProvider 把 VS Code 的文件操作映射到 examd 的 /api/fs/*。
import {
  FileChangeType,
  FileSystemProviderCapabilities,
  FileSystemProviderError,
  FileSystemProviderErrorCode,
  FileType,
  type IFileChange,
  type IFileDeleteOptions,
  type IFileOverwriteOptions,
  type IFileSystemProviderWithFileReadWriteCapability,
  type IFileWriteOptions,
  type IStat,
  type IWatchOptions
} from '@codingame/monaco-vscode-files-service-override'
import { Emitter, Event } from '@codingame/monaco-vscode-api/vscode/vs/base/common/event'
import type { IDisposable } from '@codingame/monaco-vscode-api/vscode/vs/base/common/lifecycle'
import type { URI } from '@codingame/monaco-vscode-api/vscode/vs/base/common/uri'
import { api, ApiError } from './api'

const PENDING_KEY = 'examfs.pending.v2'
const LEGACY_PENDING_PREFIX = 'examfs.pending.v1'

export interface ProviderStatus {
  online: boolean
  pending: number
  lastConflict: string | null
  lastError: string | null
  lastSavedAt: number | null
}

interface PendingWrite {
  /** 单调递增，flush 时据此判断条目是否仍是当初写的那个。 */
  id: number
  path: string
  /** base64，便于 localStorage 持久化 */
  content: string
  baseEtag?: string
  ts: number
}

function toBase64(data: Uint8Array): string {
  let binary = ''
  const chunk = 0x8000
  for (let i = 0; i < data.length; i += chunk) {
    binary += String.fromCharCode(...data.subarray(i, i + chunk))
  }
  return btoa(binary)
}

function fromBase64(value: string): Uint8Array {
  const binary = atob(value)
  const out = new Uint8Array(binary.length)
  for (let i = 0; i < binary.length; i++) out[i] = binary.charCodeAt(i)
  return out
}

function toProviderError(err: unknown): Error {
  if (err instanceof FileSystemProviderError) return err
  if (err instanceof ApiError) {
    switch (err.status) {
      case 0:
        return FileSystemProviderError.create(err.message, FileSystemProviderErrorCode.Unknown)
      case 403:
        return FileSystemProviderError.create(err.message, FileSystemProviderErrorCode.NoPermissions)
      case 404:
        return FileSystemProviderError.create(err.message, FileSystemProviderErrorCode.FileNotFound)
      case 409:
        return FileSystemProviderError.create(err.message, FileSystemProviderErrorCode.FileExists)
      case 413:
        return FileSystemProviderError.create(err.message, FileSystemProviderErrorCode.FileTooLarge)
      default:
        return FileSystemProviderError.create(`${err.status} ${err.code}: ${err.message}`, FileSystemProviderErrorCode.Unknown)
    }
  }
  return FileSystemProviderError.create(err instanceof Error ? err.message : String(err), FileSystemProviderErrorCode.Unknown)
}

export interface ProviderOptions {
  /** 当前登录学号：本地缓冲按学号隔离，换人登录不会串卷。 */
  studentNo: string
  onConflict?: (path: string) => void
  onUnauthorized?: () => void
}

export class RemoteFileSystemProvider implements IFileSystemProviderWithFileReadWriteCapability {
  readonly capabilities =
    FileSystemProviderCapabilities.FileReadWrite | FileSystemProviderCapabilities.PathCaseSensitive
  readonly onDidChangeCapabilities: Event<void> = Event.None

  private readonly changeEmitter = new Emitter<readonly IFileChange[]>()
  readonly onDidChangeFile: Event<readonly IFileChange[]> = this.changeEmitter.event

  private readonly etags = new Map<string, string>()
  // VSCode 的保存冲突检查比较 mtime：这里对每个路径返回稳定的 mtime，
  // 真正的并发控制交给 ETag/If-Match（409）。
  private readonly mtimes = new Map<string, number>()
  // 断线时用它兜底 stat，避免编辑器把本地缓冲的保存显示成失败。
  private readonly stats = new Map<string, { type: FileType; size: number }>()
  // 断线时兜底读取（内容不会比服务端更新，重连后 flush 会覆盖）。
  private readonly contents = new Map<string, Uint8Array>()
  private readonly dirs = new Map<string, [string, FileType][]>()
  private pending: PendingWrite[] = []
  private nextPendingId = 1
  private flushing = false
  private listeners = new Set<(status: ProviderStatus) => void>()
  private status: ProviderStatus = {
    online: typeof navigator === 'undefined' ? true : navigator.onLine,
    pending: 0,
    lastConflict: null,
    lastError: null,
    lastSavedAt: null
  }

  constructor(private readonly options: ProviderOptions) {
    this.loadPending()
    if (typeof window !== 'undefined') {
      window.addEventListener('online', () => {
        this.update({ online: true })
        void this.flushPending()
      })
      window.addEventListener('offline', () => this.update({ online: false }))
      // 有些环境下 online 事件不可靠，用定时器兜底重试。
      window.setInterval(() => {
        if (this.pending.length > 0) void this.flushPending()
      }, 5000)
    }
  }

  statusSnapshot(): ProviderStatus {
    return { ...this.status }
  }

  onStatus(listener: (status: ProviderStatus) => void): IDisposable {
    this.listeners.add(listener)
    listener(this.statusSnapshot())
    return {
      dispose: () => this.listeners.delete(listener)
    }
  }

  private update(patch: Partial<ProviderStatus>): void {
    this.status = { ...this.status, ...patch, pending: this.pending.length }
    for (const listener of this.listeners) listener(this.statusSnapshot())
  }

  private storageKey(): string {
    return `${PENDING_KEY}:${this.options.studentNo}`
  }

  private loadPending(): void {
    // 丢弃旧版（未按学号隔离）的缓冲，避免把上一位考生的内容写进新工作区。
    try {
      const stale: string[] = []
      for (let i = 0; i < localStorage.length; i++) {
        const key = localStorage.key(i)
        if (key === LEGACY_PENDING_PREFIX || key?.startsWith(`${LEGACY_PENDING_PREFIX}:`)) stale.push(key)
      }
      for (const key of stale) localStorage.removeItem(key)
    } catch {
      // 忽略
    }
    try {
      const raw = localStorage.getItem(this.storageKey())
      if (!raw) return
      const parsed = JSON.parse(raw) as PendingWrite[]
      if (!Array.isArray(parsed)) return
      for (const item of parsed) {
        if (typeof item?.id !== 'number') item.id = this.nextPendingId++
        if (item.id >= this.nextPendingId) this.nextPendingId = item.id + 1
      }
      this.pending = parsed
    } catch {
      this.pending = []
    }
    this.status.pending = this.pending.length
  }

  private persistPending(): void {
    try {
      if (this.pending.length === 0) localStorage.removeItem(this.storageKey())
      else localStorage.setItem(this.storageKey(), JSON.stringify(this.pending))
    } catch {
      // 配额不足时退化为内存缓冲，不影响本次会话。
    }
  }

  private queueWrite(path: string, content: Uint8Array, baseEtag?: string): void {
    const encoded = toBase64(content)
    this.contents.set(path, content)
    this.stats.set(path, { type: FileType.File, size: content.length })
    const existing = this.pending.findIndex((p) => p.path === path)
    const item: PendingWrite = { id: this.nextPendingId++, path, content: encoded, baseEtag, ts: Date.now() }
    if (existing >= 0) {
      item.baseEtag = this.pending[existing].baseEtag ?? baseEtag
      this.pending[existing] = item
    } else {
      this.pending.push(item)
    }
    this.persistPending()
    this.update({ lastError: null })
  }

  private markWritten(path: string, content: Uint8Array): void {
    this.mtimes.set(path, Math.max(Date.now(), (this.mtimes.get(path) ?? 0) + 1000))
    this.stats.set(path, { type: FileType.File, size: content.length })
    this.contents.set(path, content)
  }

  /** 成功写回后丢掉同路径的旧缓冲，避免旧内容覆盖新内容。 */
  private dropPending(path: string): void {
    const before = this.pending.length
    this.pending = this.pending.filter((p) => p.path !== path)
    if (this.pending.length !== before) {
      this.persistPending()
      this.update({})
    }
  }

  /** 只删除 id 仍然匹配的条目；条目已被更新的写入替换时返回 false。 */
  private removePending(id: number): boolean {
    const index = this.pending.findIndex((p) => p.id === id)
    if (index < 0) return false
    this.pending.splice(index, 1)
    this.persistPending()
    return true
  }

  /**
   * 写回成功后的收尾：更新 etag、删除条目、刷新本地缓存。
   * 如果写回期间同路径又有新保存（条目被替换），只把最新 etag 交给新条目，
   * 不动缓存也不删除，避免旧内容覆盖新内容。
   */
  private applyWriteResult(item: PendingWrite, entry: { etag?: string }, content: Uint8Array): void {
    const etag = entry.etag ?? ''
    const current = this.pending.find((p) => p.path === item.path)
    if (current && current.id !== item.id) {
      // 写回期间同路径又有新保存：只把最新 etag 交给新条目，不碰缓存/条目。
      this.etags.set(item.path, etag)
      current.baseEtag = etag
      return
    }
    if (this.removePending(item.id)) {
      this.etags.set(item.path, etag)
      this.markWritten(item.path, content)
      this.update({ lastSavedAt: Date.now(), lastError: null, online: true })
    }
    // 条目已被成功的直接保存删除时什么都不做，避免把 etag 回退到旧值。
  }

  /** 重连后把本地缓冲按顺序写回服务端。 */
  async flushPending(): Promise<void> {
    if (this.flushing || this.pending.length === 0) return
    this.flushing = true
    try {
      while (this.pending.length > 0) {
        const item = this.pending[0]
        const content = fromBase64(item.content)
        try {
          const entry = await api.write(item.path, content, item.baseEtag ?? '*')
          this.applyWriteResult(item, entry, content)
        } catch (err) {
          if (err instanceof ApiError && err.status === 401) {
            this.update({ lastError: '会话已过期，等待重新登录' })
            this.options.onUnauthorized?.()
            break
          }
          if (err instanceof ApiError && err.status === 409 && err.currentEtag) {
            // 条目已被更新的保存替换/删除时，直接放弃这一轮，也不弹冲突提示。
            if (!this.pending.some((p) => p.id === item.id)) break
            this.update({ lastConflict: item.path })
            this.options.onConflict?.(item.path)
            try {
              const entry = await api.write(item.path, content, err.currentEtag)
              this.applyWriteResult(item, entry, content)
              continue
            } catch (retryErr) {
              this.update({ lastError: describe(retryErr) })
              break
            }
          }
          if (err instanceof ApiError && err.status === 0) {
            this.update({ online: false, lastError: null })
            break
          }
          this.update({ lastError: describe(err) })
          break
        }
      }
    } finally {
      this.flushing = false
      this.update({})
    }
  }

  watch(_resource: URI, _opts: IWatchOptions): IDisposable {
    return { dispose() {} }
  }

  async stat(resource: URI): Promise<IStat> {
    try {
      const e = await api.stat(resource.path || '/')
      // 注意：这里不刷新 etags。etag 只在真正读取内容或自己写入成功后更新，
      // 否则保存前的 stat 会把别人的修改“洗白”，导致覆盖别人的内容。
      const serverMtime = Date.parse(e.mtime) || Date.now()
      const mtime = this.mtimes.get(e.path) ?? serverMtime
      this.mtimes.set(e.path, mtime)
      const type = e.type === 'dir' ? FileType.Directory : FileType.File
      this.stats.set(e.path, { type, size: e.size ?? 0 })
      return { type, ctime: mtime, mtime, size: e.size ?? 0 }
    } catch (err) {
      const cached = this.stats.get(resource.path)
      if (cached) {
        return {
          type: cached.type,
          ctime: this.mtimes.get(resource.path) ?? Date.now(),
          mtime: this.mtimes.get(resource.path) ?? Date.now(),
          size: cached.size
        }
      }
      throw toProviderError(err)
    }
  }

  async readdir(resource: URI): Promise<[string, FileType][]> {
    try {
      const entries = await api.list(resource.path || '/')
      const rows: [string, FileType][] = entries.map((e) => [
        e.name,
        e.type === 'dir' ? FileType.Directory : FileType.File
      ])
      this.dirs.set(resource.path || '/', rows)
      return rows
    } catch (err) {
      const cached = this.dirs.get(resource.path || '/')
      if (cached) return cached
      throw toProviderError(err)
    }
  }

  async readFile(resource: URI): Promise<Uint8Array> {
    try {
      const { data, etag, mtime } = await api.read(resource.path)
      if (etag) this.etags.set(resource.path, etag)
      if (!this.mtimes.has(resource.path)) {
        this.mtimes.set(resource.path, Date.parse(mtime) || Date.now())
      }
      this.stats.set(resource.path, { type: FileType.File, size: data.length })
      this.contents.set(resource.path, data)
      return data
    } catch (err) {
      const cached = this.contents.get(resource.path)
      if (cached) return cached
      throw toProviderError(err)
    }
  }

  async writeFile(resource: URI, content: Uint8Array, _opts: IFileWriteOptions): Promise<void> {
    const path = resource.path
    const baseEtag = this.etags.get(path)
    try {
      const entry = await api.write(path, content, baseEtag ?? '*')
      this.etags.set(path, entry.etag ?? '')
      this.dropPending(path)
      this.markWritten(path, content)
      this.update({ lastSavedAt: Date.now(), lastError: null, online: true })
      return
    } catch (err) {
      if (err instanceof ApiError && err.status === 428) {
        // 本地没有 etag（例如刚打开就保存），补一次 stat 后重试。
        try {
          const entry = await api.stat(path)
          const retry = await api.write(path, content, entry.etag ?? '*')
          this.etags.set(path, retry.etag ?? '')
          this.dropPending(path)
          this.markWritten(path, content)
          this.update({ lastSavedAt: Date.now(), lastError: null })
          return
        } catch (retryErr) {
          err = retryErr
        }
      }
      if (err instanceof ApiError && err.status === 409 && err.currentEtag) {
        this.update({ lastConflict: path })
        this.options.onConflict?.(path)
        try {
          const entry = await api.write(path, content, err.currentEtag)
          this.etags.set(path, entry.etag ?? '')
          this.dropPending(path)
          this.markWritten(path, content)
          this.update({ lastSavedAt: Date.now(), lastError: null })
          return
        } catch (retryErr) {
          err = retryErr
        }
      }
      if (err instanceof ApiError && err.status === 401) {
        // 会话过期：先缓存本地内容，重新登录后由 flushPending 写回。
        this.queueWrite(path, content, baseEtag)
        this.options.onUnauthorized?.()
        return
      }
      if (err instanceof ApiError && err.status === 0) {
        // 断线：乐观成功 + 本地缓冲，重连后 flush。
        this.queueWrite(path, content, baseEtag)
        this.update({ online: false, lastError: null })
        return
      }
      this.update({ lastError: describe(err) })
      throw toProviderError(err)
    }
  }

  async mkdir(resource: URI): Promise<void> {
    try {
      await api.mkdir(resource.path)
      this.changeEmitter.fire([{ type: FileChangeType.ADDED, resource }])
    } catch (err) {
      throw toProviderError(err)
    }
  }

  async delete(resource: URI, opts: IFileDeleteOptions): Promise<void> {
    try {
      await api.remove(resource.path, opts.recursive)
      this.etags.delete(resource.path)
      this.mtimes.delete(resource.path)
      this.changeEmitter.fire([{ type: FileChangeType.DELETED, resource }])
    } catch (err) {
      throw toProviderError(err)
    }
  }

  async rename(from: URI, to: URI, opts: IFileOverwriteOptions): Promise<void> {
    try {
      await api.rename(from.path, to.path, opts.overwrite)
      const etag = this.etags.get(from.path)
      const mtime = this.mtimes.get(from.path)
      this.etags.delete(from.path)
      this.mtimes.delete(from.path)
      if (etag) this.etags.set(to.path, etag)
      if (mtime) this.mtimes.set(to.path, mtime)
      this.changeEmitter.fire([
        { type: FileChangeType.DELETED, resource: from },
        { type: FileChangeType.ADDED, resource: to }
      ])
    } catch (err) {
      throw toProviderError(err)
    }
  }
}

function describe(err: unknown): string {
  if (err instanceof ApiError) return `${err.status} ${err.code}: ${err.message}`
  if (err instanceof Error) return err.message
  return String(err)
}
