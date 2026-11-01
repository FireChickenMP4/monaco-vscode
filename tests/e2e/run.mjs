// 浏览器端验收测试：V-01（单客户端加载耗时）、V-02、V-03、V-04、V-05、V-06。
//
// 运行前：cd server && go build -o ../bin/examd.exe ./cmd/examd
// 运行：cd tests/e2e && npm test
import { chromium } from 'playwright'
import fs from 'node:fs'
import path from 'node:path'
import { createStudent, startServer } from './server.mjs'

const results = []
let currentPage = null
const consoleErrors = []
const pageErrors = []

function log(line) {
  process.stdout.write(line + '\n')
}

async function check(id, title, fn) {
  const started = Date.now()
  try {
    const detail = await fn()
    const ms = Date.now() - started
    results.push({ id, title, ok: true, ms, detail: detail ?? '' })
    log(`[PASS] ${id} ${title} (${ms}ms)${detail ? ' :: ' + detail : ''}`)
  } catch (err) {
    const ms = Date.now() - started
    const message = err instanceof Error ? err.message : String(err)
    results.push({ id, title, ok: false, ms, detail: message })
    log(`[FAIL] ${id} ${title} (${ms}ms) :: ${message}`)
  }
}

function assert(cond, message) {
  if (!cond) throw new Error(message)
}

async function main() {
  const server = await startServer({ snapshotInterval: '2s' })
  const baseUrl = server.baseUrl
  log(`examd: ${baseUrl} (data: ${server.dataDir})`)

  const studentNo = '2026001'
  const token = 'tok-2026001'
  await createStudent(baseUrl, studentNo, 'e2e', token)

  const browser = await chromium.launch({ headless: true })
  const context = await browser.newContext({ viewport: { width: 1440, height: 900 } })
  const page = await context.newPage()
  currentPage = page
  // workbench 退出时会弹 beforeunload；默认的 dismiss 会取消导航。
  page.on('dialog', (dialog) => dialog.accept().catch(() => {}))
  page.on('console', (msg) => {
    if (msg.type() === 'error') consoleErrors.push(msg.text())
  })
  page.on('pageerror', (err) => pageErrors.push(String(err)))

  const ready = () => page.waitForFunction(() => window.__exam?.ready === true, undefined, { timeout: 90000 })
  const editorReady = (text = 'package main') =>
    page.waitForFunction(
      (needle) =>
        (document.querySelector('.view-lines')?.textContent ?? '').replace(/\u00a0/g, ' ').includes(needle),
      text,
      { timeout: 60000 }
    )
  const exam = () => page.evaluate(() => ({ ...window.__exam, flush: undefined }))
  const editorText = () =>
    page.evaluate(() => (document.querySelector('.view-lines')?.textContent ?? '').replace(/\u00a0/g, ' '))

  // 如果恢复布局后没打开任何编辑器，用 Quick Open 兜底打开 main.go。
  async function ensureMainFileOpen() {
    try {
      await editorReady('package main')
      return
    } catch {
      // 继续尝试 Ctrl+P
    }
    await page.keyboard.press('Control+P')
    await page.waitForTimeout(500)
    await page.keyboard.type('main.go')
    await page.waitForTimeout(500)
    await page.keyboard.press('Enter')
    await editorReady('package main')
  }

  // 真正重新加载文档：旧文档上的标记在新文档里必须是 undefined。
  async function hardReload() {
    await page.evaluate(() => {
      window.__staleDocument = true
    })
    await page.goto(baseUrl + '/', { waitUntil: 'domcontentloaded' })
    await page.waitForFunction(() => window.__staleDocument === undefined, undefined, { timeout: 30000 })
  }

  async function focusEditor() {
    const lines = page.locator('.view-lines').first()
    await lines.waitFor({ state: 'visible', timeout: 30000 })
    const box = await lines.boundingBox()
    assert(box, 'editor content not visible')
    // 点击第一行靠左的位置，确保焦点落在文本区而不是 minimap/滚动条。
    await page.mouse.click(box.x + 100, box.y + 8)
    await page.keyboard.press('Control+End')
  }

  async function readFile(p = '/main.go') {
    const res = await context.request.get(`${baseUrl}/api/fs/read?path=${encodeURIComponent(p)}`)
    return { status: res.status(), body: await res.text(), etag: res.headers()['etag'] }
  }

  async function writeFile(body, etag, p = '/main.go') {
    const headers = { 'Content-Type': 'text/plain' }
    if (etag) headers['If-Match'] = etag
    return await context.request.put(`${baseUrl}/api/fs/write?path=${encodeURIComponent(p)}`, { headers, data: body })
  }

  let loadMs = 0

  await check('V-01a', 'single client loads workbench and editor', async () => {
    const started = Date.now()
    await page.goto(baseUrl + '/', { waitUntil: 'domcontentloaded' })
    await page.waitForSelector('#exam-login-form', { timeout: 20000 })
    await page.fill('input[name="student_no"]', studentNo)
    await page.fill('input[name="token"]', token)
    await page.click('#exam-login-form button[type="submit"]')
    await ready()
    await ensureMainFileOpen()
    loadMs = Date.now() - started
    const text = await editorText()
    assert(text.includes('package main'), 'main.go was not opened in the editor')
    return `workbench + editor ready in ${loadMs}ms`
  })

  const marker = `// e2e-${Date.now()}`

  await check('V-02', 'edit -> autosave -> reload keeps content', async () => {
    await focusEditor()
    await page.keyboard.type(`\n${marker}`)
    await page.waitForFunction(
      (m) =>
        (document.querySelector('.view-lines')?.textContent ?? '').replace(/\u00a0/g, ' ').includes(m),
      marker,
      { timeout: 15000 }
    )
    await page.waitForTimeout(3000)
    const afterSave = await readFile()
    assert(afterSave.body.includes(marker), `server content missing marker: ${afterSave.body.slice(-120)}`)

    await hardReload()
    await ready()
    await editorReady(marker)
    const reloaded = await editorText()
    assert(reloaded.includes(marker), 'marker missing after reload')
    return 'content persisted across reload'
  })

  await check('V-03', 'server snapshot runs on interval', async () => {
    await page.waitForTimeout(5000)
    const res = await context.request.get(`${baseUrl}/api/snapshots`)
    assert(res.ok(), `snapshots api: ${res.status()}`)
    const body = await res.json()
    assert(Array.isArray(body.snapshots) && body.snapshots.length >= 1, 'no snapshots recorded')
    const latest = body.snapshots[0]
    const dir = path.join(server.dataDir, 'snapshots', studentNo, `v${String(latest.version).padStart(6, '0')}`)
    assert(fs.existsSync(path.join(dir, 'main.go')), `snapshot dir missing: ${dir}`)
    const snapContent = fs.readFileSync(path.join(dir, 'main.go'), 'utf8')
    assert(snapContent.includes(marker), 'snapshot does not contain latest content')
    return `version ${latest.version}, ${body.snapshots.length} snapshot(s), files=${latest.files}`
  })

  await check('V-04', 'path traversal is rejected', async () => {
    const res = await context.request.get(`${baseUrl}/api/fs/read?path=..%2F..%2Fexamd.db`)
    assert(res.status() === 403, `status ${res.status()}, want 403`)
    const list = await context.request.get(`${baseUrl}/api/fs/list?path=..%2F..`)
    assert(list.status() === 403, `list status ${list.status()}, want 403`)
    return '403 on ../../examd.db and ../.. listing'
  })

  const conflictMarker = `// conflict-${Date.now()}`

  await check('V-05', 'stale etag gives 409 and the UI warns', async () => {
    const before = await readFile()
    // 模拟另一个标签页先写入，服务端 etag 前进。
    const external = await writeFile(before.body + '\n// external-writer\n', before.etag)
    assert(external.ok(), `external write failed: ${external.status()}`)

    await focusEditor()
    await page.keyboard.type(`\n${conflictMarker}`)
    await page.waitForTimeout(4000)

    const state = await exam()
    assert(state.lastConflict === '/main.go', `lastConflict = ${JSON.stringify(state.lastConflict)}`)
    const warning = await page.locator('text=已被其他位置修改').count()
    assert(warning > 0, 'conflict notification not visible in the workbench')

    const after = await readFile()
    assert(after.body.includes(conflictMarker), 'local edit lost after conflict')
    return 'warning shown, local edit kept'
  })

  const offlineMarker = `// offline-${Date.now()}`

  await check('V-06', 'offline edits are buffered and synced on reconnect', async () => {
    await focusEditor()
    await context.setOffline(true)
    await page.keyboard.type(`\n${offlineMarker}`)
    await page.waitForTimeout(3500)
    const offlineState = await exam()
    assert(offlineState.pending >= 1, `pending = ${offlineState.pending}, want >= 1`)

    await context.setOffline(false)
    await page.waitForFunction(() => window.__exam?.pending === 0, undefined, { timeout: 30000 })
    await page.waitForTimeout(500)
    const after = await readFile()
    assert(after.body.includes(offlineMarker), 'offline edit was not flushed to the server')
    return 'buffered while offline, flushed after reconnect'
  })

  const unexpected = consoleErrors.filter(
    (t) =>
      !/401 \(Unauthorized\)/.test(t) &&
      // 冲突用例故意制造 409，浏览器会把失败请求打进 console。
      !/409 \(Conflict\)/.test(t) &&
      // 断网测试期间浏览器网络层必然报错，属于预期。
      !/net::ERR_INTERNET_DISCONNECTED/.test(t)
  )
  await check('V-console', 'no unexpected console/page errors', async () => {
    assert(pageErrors.length === 0, `page errors: ${pageErrors.join(' | ')}`)
    assert(unexpected.length === 0, `console errors: ${unexpected.slice(0, 5).join(' | ')}`)
    return `${consoleErrors.length} expected 401 message(s) filtered`
  })

  await browser.close()
  await server.stop()

  const failed = results.filter((r) => !r.ok)
  log('')
  log(`summary: ${results.length - failed.length}/${results.length} checks passed`)
  if (failed.length > 0) {
    for (const f of failed) log(`  failed ${f.id}: ${f.detail}`)
    process.exitCode = 1
  }
}

main().catch(async (err) => {
  log(`fatal: ${err instanceof Error ? err.stack : err}`)
  if (currentPage) {
    try {
      await currentPage.screenshot({ path: path.join(process.env.TEMP ?? '.', 'exam-e2e-failure.png'), fullPage: true })
      log('screenshot: ' + path.join(process.env.TEMP ?? '.', 'exam-e2e-failure.png'))
    } catch {
      // 截图失败不影响退出码
    }
  }
  process.exitCode = 1
})
