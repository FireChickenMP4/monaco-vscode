// 测试用的 examd 进程管理：随机端口 + 临时数据目录。
import { spawn } from 'node:child_process'
import fs from 'node:fs'
import net from 'node:net'
import os from 'node:os'
import path from 'node:path'
import { fileURLToPath } from 'node:url'

export const repoRoot = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '../..')

export const ADMIN_TOKEN = 'e2e-admin-token'

export function examdBinary() {
  const name = process.platform === 'win32' ? 'examd.exe' : 'examd'
  return process.env.EXAMD_BIN ?? path.join(repoRoot, 'bin', name)
}

export async function freePort() {
  return await new Promise((resolve, reject) => {
    const srv = net.createServer()
    srv.once('error', reject)
    srv.listen(0, '127.0.0.1', () => {
      const { port } = srv.address()
      srv.close(() => resolve(port))
    })
  })
}

export async function startServer(options = {}) {
  const port = options.port ?? (await freePort())
  const dataDir = options.dataDir ?? fs.mkdtempSync(path.join(os.tmpdir(), 'examd-e2e-'))
  const bin = examdBinary()
  if (!fs.existsSync(bin)) {
    throw new Error(`examd binary not found: ${bin} (run: cd server && go build -o ../bin/examd.exe ./cmd/examd)`)
  }
  const args = [
    'serve',
    '-addr', `127.0.0.1:${port}`,
    '-data', dataDir,
    '-web', path.join(repoRoot, 'public'),
    '-admin-token', ADMIN_TOKEN,
    '-problems', path.join(repoRoot, 'problems'),
    '-snapshot-interval', options.snapshotInterval ?? '2s'
  ]
  const proc = spawn(bin, args, { stdio: ['ignore', 'pipe', 'pipe'] })
  const logs = []
  proc.stdout.on('data', (d) => logs.push(d.toString()))
  proc.stderr.on('data', (d) => logs.push(d.toString()))

  const baseUrl = `http://127.0.0.1:${port}`
  const deadline = Date.now() + 20000
  for (;;) {
    if (proc.exitCode !== null) {
      throw new Error(`examd exited early (${proc.exitCode})\n${logs.join('')}`)
    }
    try {
      const res = await fetch(`${baseUrl}/api/health`)
      if (res.ok) break
    } catch {
      // 还没起来
    }
    if (Date.now() > deadline) throw new Error(`examd did not start\n${logs.join('')}`)
    await new Promise((r) => setTimeout(r, 200))
  }

  return {
    port,
    baseUrl,
    dataDir,
    logs,
    async stop() {
      if (proc.exitCode !== null) return
      proc.kill()
      await new Promise((resolve) => {
        const timer = setTimeout(() => {
          proc.kill('SIGKILL')
          resolve()
        }, 5000)
        proc.once('exit', () => {
          clearTimeout(timer)
          resolve()
        })
      })
    }
  }
}

export async function adminFetch(baseUrl, pathname, init = {}) {
  const res = await fetch(`${baseUrl}${pathname}`, {
    ...init,
    headers: {
      Authorization: `Bearer ${ADMIN_TOKEN}`,
      'Content-Type': 'application/json',
      ...(init.headers ?? {})
    }
  })
  return res
}

export async function createStudent(baseUrl, studentNo, name, token) {
  const res = await adminFetch(baseUrl, '/api/admin/students', {
    method: 'POST',
    body: JSON.stringify({ student_no: studentNo, name, token })
  })
  if (res.status !== 201) throw new Error(`create student failed: ${res.status} ${await res.text()}`)
  return await res.json()
}

export async function createStudents(baseUrl, count, prefix = '2026') {
  const students = []
  for (let i = 1; i <= count; i++) {
    const no = `${prefix}${String(i).padStart(4, '0')}`
    students.push(await createStudent(baseUrl, no, `考生${i}`, `tok-${no}`))
  }
  return students
}
