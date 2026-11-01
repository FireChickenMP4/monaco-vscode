// 压测脚本：模拟 N 个浏览器客户端加载 workbench，并可跑一段时间的 API 混合流量。
//
// 用法:
//   node loadtest.mjs [-n 70] [-soak 60] [-keep]
// 说明:
//   - 每个客户端完成一次登录 + 拉取 index.html 及其引用的全部静态资源，统计耗时。
//   - soak 秒数 > 0 时，再模拟持续读写，统计 5xx 数量。
//   - 这是单机模拟，不代表 70 台真实笔记本的网络与内存表现。
import { createStudents, startServer } from './server.mjs'

function parseArgs() {
  const args = process.argv.slice(2)
  const opt = { n: 70, soak: 0, keep: false }
  for (let i = 0; i < args.length; i++) {
    const a = args[i]
    if (a === '-n') opt.n = Number(args[++i])
    else if (a === '-soak') opt.soak = Number(args[++i])
    else if (a === '-keep') opt.keep = true
  }
  return opt
}

function percentile(sorted, p) {
  if (sorted.length === 0) return 0
  const idx = Math.min(sorted.length - 1, Math.ceil((p / 100) * sorted.length) - 1)
  return sorted[Math.max(0, idx)]
}

function summary(values) {
  const sorted = [...values].sort((a, b) => a - b)
  return {
    count: sorted.length,
    min: Math.round(sorted[0] ?? 0),
    p50: Math.round(percentile(sorted, 50)),
    p95: Math.round(percentile(sorted, 95)),
    max: Math.round(sorted[sorted.length - 1] ?? 0)
  }
}

async function login(baseUrl, studentNo, token) {
  const res = await fetch(`${baseUrl}/api/login`, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ student_no: studentNo, token })
  })
  if (!res.ok) throw new Error(`login ${studentNo}: ${res.status}`)
  const cookies = res.headers.getSetCookie?.() ?? []
  return cookies.map((c) => c.split(';')[0]).join('; ')
}

function assetUrls(html) {
  const urls = new Set()
  for (const m of html.matchAll(/(?:src|href)="([^"]+)"/g)) {
    const url = m[1]
    if (url.startsWith('/assets/') || url.startsWith('./assets/')) urls.add(url)
    else if (url.startsWith('/') && /\.(js|css|json|wasm|ttf)$/.test(url)) urls.add(url)
  }
  return [...urls]
}

async function fetchAll(urls, cookie) {
  for (const u of urls) {
    const res = await fetch(u.startsWith('http') ? u : u, { headers: cookie ? { Cookie: cookie } : {} })
    await res.arrayBuffer()
  }
}

async function clientLoad(baseUrl, cookie) {
  const t0 = performance.now()
  const index = await fetch(`${baseUrl}/`, { headers: { Cookie: cookie } })
  const html = await index.text()
  const tHtml = performance.now() - t0
  const urls = assetUrls(html).map((u) => new URL(u, baseUrl).toString())
  const t1 = performance.now()
  // 浏览器会并行拉取资源；这里用 4 路并发近似。
  const queue = [...urls]
  const workers = Array.from({ length: 4 }, async () => {
    for (;;) {
      const u = queue.shift()
      if (!u) return
      const res = await fetch(u, { headers: { Cookie: cookie } })
      await res.arrayBuffer()
    }
  })
  await Promise.all(workers)
  return { htmlMs: tHtml, assetsMs: performance.now() - t1, totalMs: performance.now() - t0, assets: urls.length }
}

async function main() {
  const opt = parseArgs()
  const server = await startServer({ snapshotInterval: '15s' })
  const baseUrl = server.baseUrl
  console.log(`examd: ${baseUrl}`)
  const students = await createStudents(baseUrl, opt.n)

  console.log(`\n[load] ${opt.n} clients loading workbench concurrently`)
  const started = performance.now()
  const cookies = await Promise.all(students.map((s) => login(baseUrl, s.student_no, s.token)))
  const results = await Promise.all(cookies.map((c) => clientLoad(baseUrl, c)))
  const wall = performance.now() - started

  const totals = summary(results.map((r) => r.totalMs))
  const htmls = summary(results.map((r) => r.htmlMs))
  const assets = summary(results.map((r) => r.assetsMs))
  console.log(JSON.stringify({
    clients: opt.n,
    wallMs: Math.round(wall),
    assetsPerClient: results[0]?.assets ?? 0,
    html: htmls,
    assets,
    total: totals,
    acceptanceV01: totals.p95 < 5000 ? 'PASS' : 'FAIL',
    note: 'single-machine simulation; hardware/network of 70 laptops not modelled'
  }, null, 2))

  if (opt.soak > 0) {
    console.log(`\n[soak] ${opt.n} clients, ${opt.soak}s mixed fs traffic`)
    const deadline = Date.now() + opt.soak * 1000
    const statuses = new Map()
    const latencies = []
    const failures = []
    let requests = 0
    const stop = { done: false }

    const record = (status, method, path) => {
      statuses.set(status, (statuses.get(status) ?? 0) + 1)
      if (status >= 500 && failures.length < 20) failures.push(`${status} ${method} ${path}`)
    }

    async function soakClient(i, cookie) {
      const headers = { Cookie: cookie, 'Content-Type': 'application/json' }
      let round = 0
      while (!stop.done && Date.now() < deadline) {
        round++
        const t = performance.now()
        const list = await fetch(`${baseUrl}/api/fs/list?path=/`, { headers })
        await list.arrayBuffer()
        record(list.status, 'GET', '/api/fs/list')
        const read = await fetch(`${baseUrl}/api/fs/read?path=/main.go`, { headers })
        const body = await read.text()
        const etag = read.headers.get('etag')
        record(read.status, 'GET', '/api/fs/read')
        if (round % 5 === 0 && etag) {
          const w = await fetch(`${baseUrl}/api/fs/write?path=/main.go`, {
            method: 'PUT',
            headers: { ...headers, 'If-Match': etag, 'Content-Type': 'text/plain' },
            body: body + `\n// soak ${i} ${round}\n`
          })
          record(w.status, 'PUT', '/api/fs/write')
          await w.arrayBuffer()
          requests++
        }
        requests += 2
        latencies.push(performance.now() - t)
        await new Promise((r) => setTimeout(r, 50))
      }
    }

    await Promise.all(cookies.map((c, i) => soakClient(i, c)))
    const statusesObj = Object.fromEntries([...statuses.entries()].sort((a, b) => a[0] - b[0]))
    const fiveXX = Object.entries(statusesObj).filter(([code]) => Number(code) >= 500).reduce((n, [, c]) => n + c, 0)
    console.log(JSON.stringify({
      requests,
      statuses: statusesObj,
      latency: summary(latencies),
      fiveXX,
      failures,
      acceptanceV08: fiveXX === 0 ? 'PASS (scaled-down duration)' : 'FAIL'
    }, null, 2))
  }

  if (!opt.keep) await server.stop()
  else console.log(`server kept running: ${baseUrl} data=${server.dataDir}`)
}

main().catch((err) => {
  console.error(err)
  process.exitCode = 1
})
