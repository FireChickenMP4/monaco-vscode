// 浏览器端入口：登录 -> 注册 examfs 文件系统 -> 启动 VS Code workbench。
import './workers'
import {
  IEditorService,
  INotificationService,
  LogLevel,
  Severity,
  getService,
  initialize
} from '@codingame/monaco-vscode-api'
import getBulkEditServiceOverride from '@codingame/monaco-vscode-bulk-edit-service-override'
import getConfigurationServiceOverride from '@codingame/monaco-vscode-configuration-service-override'
import getDialogsServiceOverride from '@codingame/monaco-vscode-dialogs-service-override'
import getEnvironmentServiceOverride from '@codingame/monaco-vscode-environment-service-override'
import getExplorerServiceOverride from '@codingame/monaco-vscode-explorer-service-override'
import getExtensionsServiceOverride from '@codingame/monaco-vscode-extensions-service-override'
import getFileServiceOverride, {
  registerCustomProvider
} from '@codingame/monaco-vscode-files-service-override'
import '@codingame/monaco-vscode-go-default-extension'
import '@codingame/monaco-vscode-theme-defaults-default-extension'
import '@codingame/monaco-vscode-theme-seti-default-extension'
import getKeybindingsServiceOverride from '@codingame/monaco-vscode-keybindings-service-override'
import getLanguagesServiceOverride from '@codingame/monaco-vscode-languages-service-override'
import getLifecycleServiceOverride from '@codingame/monaco-vscode-lifecycle-service-override'
import getLogServiceOverride from '@codingame/monaco-vscode-log-service-override'
import getModelServiceOverride from '@codingame/monaco-vscode-model-service-override'
import getNotificationsServiceOverride from '@codingame/monaco-vscode-notifications-service-override'
import getPreferencesServiceOverride from '@codingame/monaco-vscode-preferences-service-override'
import getQuickAccessServiceOverride from '@codingame/monaco-vscode-quickaccess-service-override'
import getSearchServiceOverride from '@codingame/monaco-vscode-search-service-override'
import getStorageServiceOverride from '@codingame/monaco-vscode-storage-service-override'
import getTextmateServiceOverride from '@codingame/monaco-vscode-textmate-service-override'
import getThemeServiceOverride from '@codingame/monaco-vscode-theme-service-override'
import getBannerServiceOverride from '@codingame/monaco-vscode-view-banner-service-override'
import getStatusBarServiceOverride from '@codingame/monaco-vscode-view-status-bar-service-override'
import getTitleBarServiceOverride from '@codingame/monaco-vscode-view-title-bar-service-override'
import getWorkbenchServiceOverride, {
  Parts,
  setPartVisibility
} from '@codingame/monaco-vscode-workbench-service-override'
import getWorkingCopyServiceOverride from '@codingame/monaco-vscode-working-copy-service-override'
import 'vscode/localExtensionHost'
import * as vscode from 'vscode'
import { ApiError, api, onUnauthorized } from './api'
import { RemoteFileSystemProvider, type ProviderStatus } from './fs-provider'

const SCHEME = 'examfs'
const ROOT = vscode.Uri.from({ scheme: SCHEME, path: '/' })

interface ExamHook {
  ready: boolean
  status: string
  pending: number
  lastConflict: string | null
  lastError: string | null
  student: string | null
  flush: () => Promise<void>
}

declare global {
  interface Window {
    __exam: ExamHook
  }
}

window.__exam = {
  ready: false,
  status: 'starting',
  pending: 0,
  lastConflict: null,
  lastError: null,
  student: null,
  flush: async () => {}
}

const banner = document.getElementById('exam-status')!
const loginRoot = document.getElementById('exam-login')!

function setBanner(text: string, kind: 'ok' | 'warn' | 'error' | 'busy' = 'ok'): void {
  banner.textContent = text
  banner.dataset.kind = kind
  window.__exam.status = kind
}

function showLogin(message?: string): Promise<{ student_no: string; name: string }> {
  return new Promise((resolve) => {
    loginRoot.hidden = false
    loginRoot.innerHTML = `
      <form id="exam-login-form" autocomplete="off">
        <h1>考场工作台</h1>
        ${message ? `<p class="exam-hint">${escapeHtml(message)}</p>` : ''}
        <label>学号 <input name="student_no" required autofocus /></label>
        <label>令牌 <input name="token" type="password" required /></label>
        <button type="submit">登录</button>
        <p class="exam-error" id="exam-login-error"></p>
      </form>`
    const form = loginRoot.querySelector('form') as HTMLFormElement
    const error = loginRoot.querySelector('#exam-login-error') as HTMLParagraphElement
    const submit = form.querySelector('button[type="submit"]') as HTMLButtonElement
    let cooldownTimer: number | undefined

    function startCooldown(seconds: number): void {
      let left = Math.max(1, Math.ceil(seconds))
      if (cooldownTimer !== undefined) window.clearInterval(cooldownTimer)
      submit.disabled = true
      const tick = (): void => {
        error.textContent = `登录尝试过多，请 ${left} 秒后重试`
        setBanner('登录被限流，请稍候', 'warn')
        if (left <= 1) {
          window.clearInterval(cooldownTimer)
          cooldownTimer = undefined
          submit.disabled = false
          error.textContent = ''
          setBanner('可以重新登录了', 'busy')
          return
        }
        left--
      }
      tick()
      if (left > 1) cooldownTimer = window.setInterval(tick, 1000)
    }

    form.addEventListener('submit', (ev) => {
      ev.preventDefault()
      if (submit.disabled) return
      const data = new FormData(form)
      const studentNo = String(data.get('student_no') ?? '').trim()
      const token = String(data.get('token') ?? '').trim()
      error.textContent = ''
      setBanner('正在登录...', 'busy')
      api
        .login(studentNo, token)
        // 用 /api/session 复核 Cookie 是否被浏览器接受（HTTP + Secure 时会被丢弃）。
        .then(() => api.session())
        .then((session) => {
          if (!session.authenticated) {
            throw new Error('登录成功但 Cookie 未被浏览器接受，请检查是否启用了 cookie-secure 但没有 HTTPS')
          }
          const me = { student_no: session.student_no, name: session.name }
          loginRoot.hidden = true
          updateUserLabel(me)
          resolve(me)
        })
        .catch((err: unknown) => {
          if (err instanceof ApiError && err.status === 429) {
            startCooldown(err.retryAfter ?? 30)
            return
          }
          error.textContent = err instanceof Error ? err.message : String(err)
          setBanner('登录失败', 'error')
        })
    })
  })
}

function escapeHtml(value: string): string {
  return value.replace(/[&<>"']/g, (ch) => {
    switch (ch) {
      case '&':
        return '&amp;'
      case '<':
        return '&lt;'
      case '>':
        return '&gt;'
      case '"':
        return '&quot;'
      default:
        return '&#39;'
    }
  })
}

function updateUserLabel(me: { student_no: string; name: string }): void {
  window.__exam.student = me.student_no
  const label = document.getElementById('exam-user')
  if (label) label.textContent = `${me.student_no}${me.name ? ' ' + me.name : ''}`
}

async function ensureSession(): Promise<{ student_no: string; name: string }> {
  const session = await api.session()
  if (session.authenticated) {
    return { student_no: session.student_no, name: session.name }
  }
  return await showLogin()
}

let reauthInProgress = false

// 会话过期：弹登录框，登录成功后把断线/过期期间缓冲的修改写回。
async function handleUnauthorized(currentProvider: RemoteFileSystemProvider): Promise<void> {
  if (reauthInProgress) return
  reauthInProgress = true
  try {
    setBanner('会话已过期，请重新登录（本地修改已缓存）', 'error')
    const me = await showLogin('会话已过期，请重新登录。本地未同步的修改会在登录后自动保存。')
    if (me.student_no !== window.__exam.student) {
      // 换人登录：缓冲按学号隔离，直接刷新页面重新初始化。
      window.location.reload()
      return
    }
    updateUserLabel(me)
    setBanner('已重新登录，正在同步本地修改...', 'busy')
    await currentProvider.flushPending()
  } finally {
    reauthInProgress = false
  }
}

function statusText(status: ProviderStatus): string {
  if (!status.online) return '离线：修改已本地缓存，恢复连接后自动同步'
  if (status.lastConflict) return `保存冲突：已保留本地版本（${status.lastConflict}）`
  if (status.pending > 0) return `待同步 ${status.pending} 个文件`
  if (status.lastSavedAt) return '已保存'
  return '就绪'
}

async function main(): Promise<void> {
  setBanner('正在连接服务器...', 'busy')
  const me = await ensureSession()
  updateUserLabel(me)

  let notification: INotificationService | undefined
  const provider = new RemoteFileSystemProvider({
    studentNo: me.student_no,
    onConflict: (path) => {
      window.__exam.lastConflict = path
      setBanner(`保存冲突：${path}`, 'warn')
      notification?.notify({
        severity: Severity.Warning,
        message: `文件 ${path} 已被其他位置修改。当前内容已按本地版本保存，请检查是否需要重新加载。`
      })
    },
    onUnauthorized: () => {
      void handleUnauthorized(provider)
    }
  })
  window.__exam.flush = () => provider.flushPending()

  // stat/read 等调用拿到 401 时也触发重新登录。
  onUnauthorized(() => {
    void handleUnauthorized(provider)
  })

  provider.onStatus((status) => {
    window.__exam.pending = status.pending
    window.__exam.lastError = status.lastError
    if (status.lastConflict) window.__exam.lastConflict = status.lastConflict
    setBanner(statusText(status), status.lastError ? 'error' : status.online ? 'ok' : 'warn')
  })

  registerCustomProvider(SCHEME, provider)

  const container = document.getElementById('workbench')!
  const defaults: Record<string, unknown> = {
    'files.autoSave': 'afterDelay',
    'files.autoSaveDelay': 1000,
    'files.eol': '\n',
    'editor.tabSize': 4,
    'editor.detectIndentation': false,
    'editor.minimap.enabled': false,
    'editor.experimental.asyncTokenization': true,
    'workbench.startupEditor': 'none',
    'workbench.colorTheme': 'Default Dark Modern',
    'workbench.iconTheme': 'vs-seti',
    'workbench.tips.enabled': false,
    'workbench.enableExperiments': false,
    'workbench.welcomePage.walkthroughs.openOnInstall': false,
    'extensions.autoUpdate': false,
    'extensions.ignoreRecommendations': true,
    'update.mode': 'none',
    'telemetry.telemetryLevel': 'off',
    'security.workspace.trust.enabled': false,
    'window.title': '考场工作台 — ${activeEditorShort}'
  }

  await initialize(
    {
      ...getBulkEditServiceOverride(),
      ...getModelServiceOverride(),
      ...getFileServiceOverride(),
      ...getConfigurationServiceOverride(),
      ...getKeybindingsServiceOverride(),
      ...getLanguagesServiceOverride(),
      ...getTextmateServiceOverride(),
      ...getThemeServiceOverride(),
      ...getPreferencesServiceOverride(),
      ...getStorageServiceOverride(),
      ...getLogServiceOverride(),
      ...getNotificationsServiceOverride(),
      ...getDialogsServiceOverride(),
      ...getQuickAccessServiceOverride(),
      ...getSearchServiceOverride(),
      ...getExplorerServiceOverride(),
      ...getWorkingCopyServiceOverride(),
      ...getLifecycleServiceOverride(),
      ...getEnvironmentServiceOverride(),
      ...getExtensionsServiceOverride({ enableWorkerExtensionHost: false }),
      ...getBannerServiceOverride(),
      ...getStatusBarServiceOverride(),
      ...getTitleBarServiceOverride(),
      ...getWorkbenchServiceOverride()
    },
    container,
    {
      enableWorkspaceTrust: false,
      workspaceProvider: {
        trusted: true,
        async open(): Promise<boolean> {
          return true
        },
        workspace: { folderUri: ROOT }
      },
      defaultLayout: {
        views: [{ id: 'workbench.view.explorer' }],
        editors: [{ uri: ROOT.with({ path: '/main.go' }), viewColumn: 1 }]
      },
      productConfiguration: {
        nameShort: 'Exam',
        nameLong: 'Exam Workbench',
        applicationName: 'exam-workbench'
      },
      configurationDefaults: defaults,
      developmentOptions: { logLevel: LogLevel.Warning }
    },
    { userHome: vscode.Uri.from({ scheme: 'file', path: '/' }) }
  )

  notification = await getService(INotificationService)

  // 恢复布局时可能没有打开任何编辑器；兜底打开 main.go。
  const editorService = await getService(IEditorService)
  if (editorService.editors.length === 0) {
    await editorService.openEditor({
      resource: vscode.Uri.from({ scheme: SCHEME, path: '/main.go' }),
      options: { pinned: true }
    })
  }

  // 先尝试把上次断线留下的缓冲写完。
  void provider.flushPending()

  // 关闭终端/输出面板，减少误触与内存占用（PLAN Q-04）。
  setPartVisibility(Parts.PANEL_PART, false)

  document.getElementById('exam-logout')!.addEventListener('click', () => {
    void api.logout().then(() => window.location.reload())
  })
  document.getElementById('exam-topbar')!.hidden = false
  window.__exam.ready = true
  setBanner(statusText(provider.statusSnapshot()))
}

main().catch((err: unknown) => {
  console.error(err)
  const message = err instanceof Error ? err.message : String(err)
  setBanner(`启动失败：${message}`, 'error')
  window.__exam.lastError = message
})
