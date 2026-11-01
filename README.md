# monaco-vscode-api + 文件 API 方案

> 状态：可运行原型（examd + workbench + 判题），验收项见 `TODO.md`
> 适用：需要"代码始终在服务端"、且接受数周集成工作时
> 详细计划：`PLAN.md`

## 一句话

浏览器里跑完整的 VS Code workbench（`@codingame/monaco-vscode-api`），文件通过 `FileSystemProvider` 存到服务端，需要时在服务端沙箱里运行和判题。没有每用户 Node 进程。

## 和 Git + Agent 的区别

| | 本方案 | Git + Agent |
|---|---|---|
| 编辑器 | 浏览器里的 VS Code workbench | 考生自己的 VS Code/GoLand |
| 文件 | 服务端文件 API | 本机，push 后到服务端 |
| 运行 | 服务端沙箱（可选） | 本机 Go |
| 代码留存 | 强：服务端快照 | 弱：agent 可停 |
| 客户端安装 | 无 | agent + Go |
| 建设量 | 数周 | 1-2 周 |
| Go 补全 | 没有 gopls | 考生本机完整 |
| 服务器内存 | 约 300MB + 沙箱 | 约 300MB + 可选判题 |

## 什么时候选它

- 你明确要"代码从第一秒就在服务器"，不接受 agent 可停。
- 你不想让 70 台笔记本装 agent、装 Go。
- 你愿意投入数周做前端集成，并接受没有 Go 语言服务。
- 你有 8GB 云服务器或 Mac；不需要 70 个 code-server。

## 它不解决什么

- 截图、拍照、切网搜索：一样挡不住。
- 粘贴 AI 代码：只能靠过程记录和现场复核。
- 断线只读需要主动实现，不是自带。

先看 `PLAN.md` 的工作量和风险，再决定要不要开这条线。

## 目录结构

```
monaco-vscode/
├── web/                 # Vite + TS 前端（monaco-vscode-api workbench）
│   ├── src/main.ts      # 登录、启动 workbench、状态条
│   ├── src/fs-provider.ts  # examfs FileSystemProvider（ETag/If-Match/断线缓冲）
│   ├── src/workers.ts   # 编辑器与 TextMate worker
│   └── vite.config.ts   # 构建到 ../public
├── server/              # Go 后端 examd
│   ├── cmd/examd       # serve / addstudent / provision / snapshot / judge
│   └── internal/        # store(SQLite) / workspace / snapshot / judge / httpapi
├── problems/            # 题目定义（JSON，含用例）
├── public/              # 前端构建产物（examd 直接托管）
├── tests/e2e/           # Playwright 验收测试 + 压测脚本
├── deploy/              # systemd / nginx 示例
└── bin/                 # examd 可执行文件
```

## 快速开始

```powershell
# 1. 构建前端（Windows 原生，无需 WSL）
cd web
npm install
npm run build          # 输出到 ../public

# 2. 构建后端
cd ../server
go build -o ../bin/examd.exe ./cmd/examd

# 3. 建考生账号（会自动生成工作区模板）
cd ..
.\bin\examd.exe addstudent -data .\data -no 2026001 -name 张三
# 输出里的 token 就是登录令牌

# 4. 启动
.\bin\examd.exe serve -data .\data -web .\public -admin-token <管理端令牌> -problems .\problems
```

浏览器打开 `http://<服务器>:8080/`，输入学号和令牌即可进入工作台。

已有考生需要补工作区模板时：

```powershell
.\bin\examd.exe provision -data .\data
```

## 配置

| 参数 / 环境变量 | 默认 | 说明 |
|---|---|---|
| `-addr` / `EXAM_ADDR` | `:8080` | 监听地址 |
| `-data` / `EXAM_DATA` | `./data` | 数据目录（SQLite、工作区、快照） |
| `-web` / `EXAM_WEB` | `./public` | 前端产物目录 |
| `-admin-token` / `EXAM_ADMIN_TOKEN` | 自动生成 | 为空时生成并写入 `data/admin_token` |
| `-session-ttl` | `12h` | 登录会话有效期 |
| `-snapshot-interval` | `15s` | 工作区快照周期 |
| `-max-file` | `2MB` | 单文件上限 |
| `-judge-image` / `EXAM_JUDGE_IMAGE` | `exam-go:1.26` | 判题镜像 |
| `-problems` / `EXAM_PROBLEMS` | `./problems` | 题目目录 |
| `-judge-workers` | `2` | 判题并发 |
| `-admin-cidr` / `EXAM_ADMIN_CIDR` | 空（不限） | 允许访问管理端接口的网段，逗号分隔 |
| `-trust-proxy` | `false` | 从 `X-Forwarded-For` 取客户端 IP |

## 文件 API

| 方法 | 路径 | 说明 |
|---|---|---|
| GET | `/api/fs/list?path=` | 列目录 |
| GET | `/api/fs/stat?path=` | 元数据 + `ETag` |
| GET | `/api/fs/read?path=` | 读文件（支持 `If-None-Match` -> 304） |
| PUT | `/api/fs/write?path=` | 写文件，必须带 `If-Match`（新建用 `*`） |
| POST | `/api/fs/mkdir?path=` | 建目录 |
| POST | `/api/fs/rename` | `{"from","to","overwrite"}` |
| DELETE | `/api/fs/delete?path=&recursive=1` | 删除 |

- 路径一律相对工作区根，任何 `..`、盘符、ADS 都被拒绝（403）。
- 文件已存在但缺 `If-Match` -> 428；etag 不匹配 -> 409，响应体带 `current_etag`。
- 每个写操作进 `audit` 表。

## 测试

```powershell
# 后端单测（含 Docker 判题集成测试，无 Docker 时自动跳过）
cd server
go test ./...

# 浏览器验收（会自动起一个临时 examd）
cd ../tests/e2e
npm install
npx playwright install chromium
node run.mjs

# 70 客户端加载 + 缩时压测
node loadtest.mjs -n 70 -soak 60
```

## 部署

```bash
# Linux
cd web && npm ci && npm run build && cd ..
cd server && go build -o ../bin/examd ./cmd/examd && cd ..
sudo cp deploy/examd.service /etc/systemd/system/
sudo systemctl daemon-reload && sudo systemctl enable --now examd
```

`deploy/nginx.conf` 给了前置 nginx 的示例（静态资源 + 反代 `/api`，并开启 gzip）。

## 与 Git + Agent 的关系

`examd` 与 git-agent 方案的 `examd` 共用“学生、快照、判题”的模型和 SQLite 表结构思路，但按本目录独立部署，互不依赖；需要合并时可以只搬 `internal/store` 与 `internal/judge`。

