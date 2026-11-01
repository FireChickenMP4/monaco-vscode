# TODO / 后续追踪

> 对应 `PLAN.md` 的验收项与开放问题。
> 最近更新：2026-11-01

## 1. 验收项现状

| 编号 | 验收项 | 状态 | 证据 |
|---|---|---|---|
| V-01 | 70 台浏览器同时加载 workbench，p95 < 5 秒 | 通过（单机模拟） | `loadtest.mjs -n 70`：p95 1.82s、p50 1.61s、max 1.83s（70 个客户端同时拉取 index.html + 11MB 主包，服务端 gzip 结果内存缓存） |
| V-02 | 编辑、保存、刷新后内容一致 | 通过 | `tests/e2e/run.mjs` V-02；`server/internal/httpapi/server_test.go` |
| V-03 | 服务端快照每 15 秒生成 | 通过 | `server/internal/snapshot/snapshot_test.go`；e2e V-03（测试用 2s 间隔，2 个版本、内容含最新编辑） |
| V-04 | 路径越权返回 403 | 通过 | `server/internal/workspace/workspace_test.go`；`server/internal/httpapi/server_test.go`；e2e V-04 |
| V-05 | 写冲突返回 409，前端提示 | 通过 | `server/internal/httpapi/server_test.go`；e2e V-05（409 + workbench 通知 + 本地内容保留） |
| V-06 | 断线后重连，内容不丢 | 通过 | e2e V-06（setOffline -> 编辑 -> pending=1 -> 恢复后 pending=0 且服务端内容含新编辑） |
| V-07 | 判题结果与参考解一致 | 通过 | `server/internal/judge/judge_docker_test.go`（Docker `exam-go:1.26`，3/3 用例）；`judge_test.go` 覆盖 WA/编译失败/未知题目 |
| V-08 | 70 客户端 2 小时无 5xx | 通过（缩时） | `loadtest.mjs -n 70 -soak 45`：111,817 次请求全部 200，0 个 5xx，API p95 24ms；2 小时真机长跑仍待补 |

## 2. 已实现

- 浏览器内 VS Code workbench（`@codingame/monaco-vscode-api` 38.0.7，Vite 8 构建，Windows 原生可构建）
- 自定义 `examfs` FileSystemProvider（list/stat/read/write/mkdir/rename/delete，ETag + `If-Match`）
- 断线本地缓冲（localStorage）与重连自动回写；冲突时保留本地内容并弹出通知
- 登录令牌 + HttpOnly/SameSite=Lax 会话 Cookie；学号工作区隔离；来源网段限制
- 审计表（登录、写、删、重命名、快照、判题）
- 15 秒周期快照 + 手动快照 + 快照回放（admin）
- Docker 判题（编译容器/运行容器分离，宿主比较输出），每人每题 20 次限流，异步队列
- 静态资源服务（gzip 结果内存缓存、`/assets/*` 长缓存、SPA 回退）
- 管理端页面 `/admin.html`：学生列表/新建/重置令牌/停用、快照查询与回放、判题提交与记录、审计查询
- Go 语法高亮（内置 go 扩展 + TextMate worker）

## 3. 后续工作（按优先级）

### P0 上线前必须

1. **真实环境压测**：70 台笔记本（或等效网络条件）跑满 2 小时，统计 5xx 与 p95；当前只有单机模拟（p95 1.82s / 45s soak 0 个 5xx）。
   建议同时采集浏览器内存（`performance.memory`）与首屏时间。
2. **TLS 与 Cookie Secure**：内网若加 HTTPS，把 `setCookie` 的 `Secure` 置为 true 并核对 SameSite 行为。
3. **管理端页面**：目前只有 `/api/admin/*` 接口；需要一个最小管理页（学生列表、令牌重置、快照回放、判题记录）。
4. **Docker 镜像与题目下发**：把 `exam-go:1.26` 的构建脚本、题目 JSON 下发方式写进部署文档。

### P1 体验与可靠性

5. **隐藏终端/扩展面板**（PLAN Q-04）：通过配置/布局默认值关闭终端与扩展视图，降低误触与内存占用。
6. **断线只读**（PLAN Q-02）：目前是“本地继续编辑 + 回连合并”；若考场要求断线只读，需要加网络状态监听并切只读。
7. **冲突策略可配置**：现在是“本地优先 + 提示”，可加“服务端优先/手动选择”策略开关。
8. **大文件与二进制**：单文件上限 2MB；若要支持图片等，补 MIME 处理与大小分级。
9. **前端体积**：主包约 11MB（gzip 2.7MB）；可评估按需加载语言/主题扩展以缩短首屏。

### P2 可选能力

10. **Go LSP**（PLAN Q-01）：接 gopls（每用户进程，内存成本高）或等 WASM 版；当前只有语法高亮。
11. **在线运行**（PLAN Q-03）：学生自助运行样例；需要额外的运行队列与更严格的资源限制。
12. **审计导出**：把 `audit` 表导出 CSV/Excel，便于考后复核。
13. **快照对比/回放 UI**：现在只能通过 admin API 回放。
14. **会话过期处理**：写操作遇到 401 时目前只在状态条显示错误；应弹出重新登录框并保留本地缓冲。
15. **登录限流**：令牌登录没有失败次数限制，建议按 IP + 学号做简单限速。

## 4. 已知限制

- monaco-vscode-api 38.0.7 的工作台包版本比 `api` 包低一个小版本（镜像站同步滞后），目前组合可正常构建运行。
- `?worker` 方式打包 worker 时，Vite 需要 `worker.format: 'es'`；已在 `web/vite.config.ts` 固定。
- 工作区根目录就是 `/`，浏览器地址栏显示 `examfs:///`；如果需要更友好的名字，可换成带 authority 的 URI。
- SQLite 使用 WAL + `busy_timeout`，写入是串行的；判题大并发时应单独部署。
- 判题使用宿主 `docker` 命令，Windows 下路径由 Docker Desktop 转换，Linux 部署行为一致但未在本机验证。
