# monaco-vscode-api + 文件 API 实施计划

> 版本：v0.1（草案）
> 日期：2026-11-01
> 状态：已实现最小可用版本（examd + workbench + 快照 + 判题），验收进度见 `TODO.md`
> 合并版参考：`../3.md` 的附录 F

---

## 1. 目标

在考场局域网内，让考生用浏览器打开一个完整的 VS Code 界面：

- 编辑器在浏览器里跑
- 文件通过文件 API 存到服务端
- 需要时在服务端沙箱里运行、判题
- 没有每用户 Node 进程；服务器只提供静态资源和文件 API

---

## 2. 与 Git + Agent 的边界

| 项 | 本方案 | Git + Agent |
|---|---|---|
| 代码位置 | 服务端 | 本机，push 后到服务端 |
| 编辑器 | monaco-vscode-api workbench | 考生自己的 IDE |
| 运行 | 服务端沙箱 | 本机 |
| 客户端安装 | 浏览器 | agent + Go |
| 强制留存 | 强 | 弱 |
| 建设量 | 数周 | 1-2 周 |

---

## 3. 架构

```
考生浏览器
  └─ VS Code workbench（monaco-vscode-api）
        │ HTTP/WebSocket
        ▼
AP（无外网）── 路由器 ── 服务器
                          ├─ examd :8080
                          │   ├─ 静态资源（workbench）
                          │   ├─ 文件 API
                          │   ├─ 登录/会话
                          │   └─ 快照
                          ├─ SQLite
                          └─ 判题（Docker，可选）
```

- 静态资源由 examd 或前置 nginx 提供。
- 文件 API 与 workbench 同源，避免 CORS。
- 不依赖公网；工作台资源在局域网加载。

---

## 4. 技术选型

| 项 | 选择 | 说明 |
|---|---|---|
| Workbench | `@codingame/monaco-vscode-api` | MIT；516 stars；完整 VS Code 服务与扩展宿主 |
| 文件系统 | `FileSystemProvider` | 接到 examd 的 `/api/fs/*` |
| 语言 | 默认 Go 语法高亮 | 没有 gopls；补全需另接 LSP |
| 前端构建 | Vite + TypeScript | 与现有前端栈一致 |
| 后端 | Go + SQLite | 与 git-agent 方案共用 |
| 判题 | Docker `exam-go:1.26` | 与 git-agent 方案共用 |

限制：

- monaco-vscode-api 构建只支持 Linux/macOS；Windows 用 WSL。
- 工作台首次加载 10-20MB；70 台同时打开要压测。
- 每个浏览器标签页 100-300MB 内存；老笔记本可能吃力。
- Go 没有 gopls；要补全就得另起 LSP 服务（每用户内存）或等 WASM 版。

---

## 5. 文件 API

| 方法 | 路径 | 说明 |
|---|---|---|
| GET | /api/fs/list?path= | 列目录 |
| GET | /api/fs/stat?path= | 文件元数据、etag |
| GET | /api/fs/read?path= | 读文件 |
| PUT | /api/fs/write?path= | 写文件，带 `If-Match` |
| POST | /api/fs/mkdir | 建目录 |
| POST | /api/fs/rename | 重命名 |
| DELETE | /api/fs/delete | 删除 |

- 只允许访问考生自己的工作区；路径规范化，拒绝 `..`。
- 写接口带 `If-Match`/etag，冲突返回 409。
- 前端防抖 1 秒；服务端每 15 秒落一次快照。
- 断线时前端本地缓冲；重连后按 etag 合并。

---

## 6. 登录与权限

- 令牌登录；`HttpOnly; SameSite=Lax` 会话 Cookie。
- 每个考生一个工作区目录；只允许访问自己的目录。
- 管理端独立账号；来源网段限制。
- 审计记录所有写操作。

---

## 7. 自动保存与快照

- 编辑器每次保存/防抖 -> `PUT /api/fs/write`。
- 服务端每 15 秒对工作区做一次快照，存 `data/workspaces/<student_no>/`。
- 快照记录 `version` 与 hash；回放/复核用。
- 断线只读（可选）：WebSocket 断开时前端切只读；重连后恢复。
- 不实现断线只读时，浏览器仍可本地编辑，回连后同步。

---

## 8. 判题

- 与 git-agent 方案共用 `examd judge`。
- 输入是服务端工作区，不是 Git 仓库；直接打包工作区。
- 编译容器 / 运行容器分开；期望输出在宿主。
- 可选推送/保存触发：每次保存入队，限流每人每题 20 次。
- 默认考后批量，避免考试当天占资源。

---

## 9. 风险与工作量

| 项 | 说明 |
|---|---|
| 集成 | 文件 API、认证、快照、workbench 构建、版本锁定；数周 |
| 浏览器性能 | 首次 10-20MB；70 台；每 tab 100-300MB |
| Go 语言服务 | 没有 gopls；只有语法高亮 |
| 文件 API 可靠性 | 断线、冲突、重试、本地缓冲 |
| 维护 | 前端复杂，换届交接难 |
| 安全 | 不防截图/切网；只保证代码在服务端 |

工作量估算：2 人、4-8 周；比 Git + Agent 多 3-6 周。

---

## 10. 验收

| 编号 | 验收项 |
|---|---|
| V-01 | 70 台浏览器同时加载 workbench，p95 < 5 秒 |
| V-02 | 编辑、保存、刷新后内容一致 |
| V-03 | 服务端快照每 15 秒生成 |
| V-04 | 路径越权返回 403 |
| V-05 | 写冲突返回 409，前端提示 |
| V-06 | 断线后重连，内容不丢 |
| V-07 | 判题结果与参考解一致 |
| V-08 | 70 客户端 2 小时无 5xx |

---

## 11. 开放问题

| 编号 | 问题 | 默认 |
|---|---|---|
| Q-01 | 是否接 gopls | 不接 |
| Q-02 | 断线是否只读 | 不做 |
| Q-03 | 是否做在线运行 | 不做 |
| Q-04 | 是否保留终端/扩展面板 | 关闭 |
| Q-05 | 与 Git + Agent 是否共用 examd | 共用 |

---

## 12. 下一步

1. 跑通 monaco-vscode-api demo
2. 实现最小 FileSystemProvider
3. 登录 + 工作区隔离
4. 保存/加载/快照
5. 70 客户端加载压测
6. 再决定是否值得替换 Git + Agent
