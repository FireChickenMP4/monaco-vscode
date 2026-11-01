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
- 管理端页面 `/admin.html`：学生列表/新建/重置令牌/停用、快照查询与回放、判题提交与记录、审计查询与 CSV 导出
- 会话过期处理：写操作遇到 401 会先缓存本地内容、弹出重新登录框，登录后自动写回（缓冲按学号隔离）
- 登录失败限流：按「IP+学号」硬封禁（默认 10 次/5 分钟），IP 维度阈值 10 倍且**凭据正确时不受限流影响**，避免锁死整间教室
- 会话 Cookie 可配置 `Secure`（`-cookie-secure`，或 `-trust-proxy` 时按 `X-Forwarded-Proto` 自动判断），`X-Forwarded-For` 取最右一跳
- 审计 CSV 导出做了公式注入转义，并用 `X-Row-Count` / `X-Truncated` 标明是否截断；JSON 审计接口单独限制 1000 条
- 登录框遇到 429 会按 `Retry-After` 倒计时禁用提交
- 默认隐藏终端/输出面板（`setPartVisibility(PANEL_PART, false)`）
- 判题镜像构建脚本 `deploy/docker/exam-go.Dockerfile`（`GOTOOLCHAIN=local`，离线可用）
- Go 语法高亮（内置 go 扩展 + TextMate worker）

## 3. 后续工作（按优先级）
### P0 上线前必须

1. **真实环境压测**：70 台笔记本（或等效网络条件）跑满 2 小时，统计 5xx 与 p95；当前只有单机模拟（p95 1.82s / 45s soak 0 个 5xx）。
   建议同时采集浏览器内存（`performance.memory`）与首屏时间。
2. **HTTPS 落地**：`-cookie-secure` 与 `X-Forwarded-Proto` 已支持，实际证书与 nginx TLS 配置按考场网络补。

### P1 体验与可靠性

3. **扩展视图彻底隐藏**（PLAN Q-04）：终端/输出面板已默认关闭；Extensions 视图仍在（没有 gallery 时是空列表），如需彻底移除要另做 view container 裁剪。
4. **断线只读**（PLAN Q-02）：目前是“本地继续编辑 + 回连合并”；若考场要求断线只读，需要加网络状态监听并切只读。
5. **冲突策略可配置**：现在是“本地优先 + 提示”，可加“服务端优先/手动选择”策略开关。
6. **大文件与二进制**：单文件上限 2MB；若要支持图片等，补 MIME 处理与大小分级。
7. **前端体积**：主包约 11MB（gzip 2.7MB）；可评估按需加载语言/主题扩展以缩短首屏。

### P2 可选能力

8. **Go LSP**（PLAN Q-01）：接 gopls（每用户进程，内存成本高）或等 WASM 版；当前只有语法高亮。
9. **在线运行**（PLAN Q-03）：学生自助运行样例；需要额外的运行队列与更严格的资源限制。
10. **快照对比 UI**：现在能回放，但没有“两个版本 diff”的界面。
11. **审计导出定时任务**：目前是管理端手动导出 CSV；如需考后自动归档，可加定时导出。
12. **判题容器加固**：已限制网络/内存/CPU/PID，可再加只读根文件系统与 `--user`（需要同步调整产物目录权限）。

## 4. 已知限制

- monaco-vscode-api 38.0.7 的工作台包版本比 `api` 包低一个小版本（镜像站同步滞后），目前组合可正常构建运行。
- `?worker` 方式打包 worker 时，Vite 需要 `worker.format: 'es'`；已在 `web/vite.config.ts` 固定。
- 工作区根目录就是 `/`，浏览器地址栏显示 `examfs:///`；如果需要更友好的名字，可换成带 authority 的 URI。
- SQLite 使用 WAL + `busy_timeout`，写入是串行的；判题大并发时应单独部署。
- 判题使用宿主 `docker` 命令，Windows 下路径由 Docker Desktop 转换，Linux 部署行为一致但未在本机验证。

## 5. 决策记录（代码审查后确定，可按需调整）

1. **登录限流维度**：硬封禁只针对「IP+学号」，IP 维度阈值放宽到 10 倍且凭据正确时直接放行。
   - 理由：考场里一个考生输错不能连带锁死同 NAT 下的其他人，也不能把「会话过期后的正确重登」挡在门外。
   - 代价：攻击者仍可对单个学号做有限次猜测（10 次/5 分钟），但无法借此瘫痪考场。
2. **`X-Forwarded-For` 取最右一跳**：要求反代追加（nginx `$proxy_add_x_forwarded_for`），不再信任客户端自带的前缀。
3. **审计导出完整性**：CSV 默认最多 5 万条，响应头 `X-Truncated: true` 表明还有更早记录；JSON 审计接口限制 1000 条。
   - 若要求「审计必须可全量导出」，需要改成按 id 区间分页拉取（会多一个迭代）。
4. **本地缓冲按学号隔离**：`examfs.pending.v2:<学号>`；换人登录不会把上一位考生的内容写进新工作区。
   - 会话过期后如果换人登录，前端会直接刷新页面重新初始化。
