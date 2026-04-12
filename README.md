# Sandbox

安全、API 驱动的代码沙箱执行服务，基于 Go 构建。为运行不受信任的 Python、Node.js/TypeScript 和 Bash 代码提供隔离环境，支持细粒度的资源控制与网络隔离。

## 特性

- **统一多语言运行时** — 单一容器同时支持 Python、Node.js/TypeScript、Bash，执行时指定语言
- **双运行时后端** — Docker（通过 Gateway Sidecar 实现网络过滤）和 Kubernetes（通过 NetworkPolicy 实现网络隔离）
- **RESTful API** — 一次性执行、持久化沙箱、同步/流式（SSE）输出
- **预热容器池** — 预热容器池实现低延迟分配，启动时自动清理上次遗留的孤儿容器
- **安全隔离** — 资源限制（CPU、内存、PID、磁盘）、只读根文件系统、Seccomp 安全配置、网络白名单、API Key 认证、速率限制
- **文件操作** — 沙箱内文件的上传、下载和列表查看
- **工作空间** — 基于 ScopedFS 的持久化工作空间，支持挂载/卸载/增量同步，路径限定防止目录逃逸
- **会话持久化** — Persistent 模式沙箱元数据存储到 Redis，API 重启后自动恢复
- **文件存储** — 可插拔后端：Local、S3、COS、OBS、OSS、MinIO
- **Helm Chart** — 生产级 Kubernetes 部署，支持 HPA 自动伸缩

## 快速开始

### Docker Compose

```bash
cd docker
cp .env.example .env   # 编辑 .env 配置 API Key 等参数
docker-compose up -d
```

服务启动后，API 默认监听 `http://localhost:8080`。

### API 示例

**一次性执行代码：**

```bash
curl -X POST http://localhost:8080/api/v1/execute \
  -H "Authorization: Bearer your-api-key" \
  -H "Content-Type: application/json" \
  -d '{"language":"python","code":"print(\"Hello Sandbox!\")"}'
```

**创建持久化沙箱：**

```bash
# 创建沙箱（不需要指定语言，同一沙箱可运行任意语言）
curl -X POST http://localhost:8080/api/v1/sandboxes \
  -H "Authorization: Bearer your-api-key" \
  -H "Content-Type: application/json" \
  -d '{"mode":"persistent"}'

# 执行 Python 代码
curl -X POST http://localhost:8080/api/v1/sandboxes/<id>/exec \
  -H "Authorization: Bearer your-api-key" \
  -H "Content-Type: application/json" \
  -d '{"language":"python","code":"x = 42\nprint(x)"}'

# 同一沙箱执行 Node.js 代码
curl -X POST http://localhost:8080/api/v1/sandboxes/<id>/exec \
  -H "Authorization: Bearer your-api-key" \
  -H "Content-Type: application/json" \
  -d '{"language":"nodejs","code":"console.log(Array.from({length:5}, (_,i) => i*i))"}'

# 同一沙箱执行 Bash 命令
curl -X POST http://localhost:8080/api/v1/sandboxes/<id>/exec \
  -H "Authorization: Bearer your-api-key" \
  -H "Content-Type: application/json" \
  -d '{"language":"bash","code":"echo $PATH && python3 --version && node --version"}'

# 销毁沙箱
curl -X DELETE http://localhost:8080/api/v1/sandboxes/<id> \
  -H "Authorization: Bearer your-api-key"
```

**安装依赖（支持混合 pip + npm）：**

```bash
curl -X POST http://localhost:8080/api/v1/sandboxes \
  -H "Authorization: Bearer your-api-key" \
  -H "Content-Type: application/json" \
  -d '{
    "mode":"persistent",
    "dependencies":[
      {"name":"flask","version":"3.0.0","manager":"pip"},
      {"name":"express","version":"4.18.2","manager":"npm"}
    ]
  }'
```

**创建带工作空间的沙箱：**

```bash
# 创建沙箱并挂载工作空间（自动将存储后端的文件同步到容器 /workspace）
curl -X POST http://localhost:8080/api/v1/sandboxes \
  -H "Authorization: Bearer your-api-key" \
  -H "Content-Type: application/json" \
  -d '{"mode":"persistent","workspace_path":"user123/project-a"}'

# 也可以创建沙箱后再动态挂载
curl -X POST http://localhost:8080/api/v1/sandboxes/<id>/workspace/mount \
  -H "Authorization: Bearer your-api-key" \
  -H "Content-Type: application/json" \
  -d '{"root_path":"user123/project-a"}'

# 手动同步：将容器内的修改增量保存回存储（仅写入变更文件）
curl -X POST http://localhost:8080/api/v1/sandboxes/<id>/workspace/sync \
  -H "Authorization: Bearer your-api-key" \
  -H "Content-Type: application/json" \
  -d '{"direction":"from_container"}'

# 卸载工作空间（自动增量同步回存储）
curl -X POST http://localhost:8080/api/v1/sandboxes/<id>/workspace/unmount \
  -H "Authorization: Bearer your-api-key"
```

**流式输出（SSE）：**

```bash
curl -X POST http://localhost:8080/api/v1/execute/stream \
  -H "Authorization: Bearer your-api-key" \
  -H "Content-Type: application/json" \
  -d '{"language":"python","code":"import time\nfor i in range(5):\n    print(i)\n    time.sleep(1)"}'
```

## API 接口

| 方法 | 路径 | 说明 |
|------|------|------|
| GET | `/health` | 健康检查 |
| POST | `/api/v1/execute` | 一次性执行代码 |
| POST | `/api/v1/execute/stream` | 一次性执行代码（流式输出） |
| POST | `/api/v1/sandboxes` | 创建沙箱 |
| GET | `/api/v1/sandboxes/:id` | 获取沙箱信息 |
| DELETE | `/api/v1/sandboxes/:id` | 销毁沙箱 |
| POST | `/api/v1/sandboxes/:id/exec` | 在沙箱中执行代码 |
| POST | `/api/v1/sandboxes/:id/exec/stream` | 在沙箱中执行代码（流式输出） |
| POST | `/api/v1/sandboxes/:id/files/upload` | 上传文件到沙箱 |
| GET | `/api/v1/sandboxes/:id/files/download` | 从沙箱下载文件 |
| GET | `/api/v1/sandboxes/:id/files/list` | 列出沙箱中的文件 |
| PUT | `/api/v1/sandboxes/:id/network` | 更新沙箱网络配置 |
| POST | `/api/v1/sandboxes/:id/workspace/mount` | 挂载工作空间 |
| POST | `/api/v1/sandboxes/:id/workspace/unmount` | 卸载工作空间 |
| POST | `/api/v1/sandboxes/:id/workspace/sync` | 手动同步工作空间 |
| GET | `/api/v1/sandboxes/:id/workspace/info` | 获取工作空间信息 |

## 配置

通过 `configs/config.yaml` 或环境变量配置，环境变量以 `SANDBOX_` 为前缀：

```bash
SANDBOX_SERVER_PORT=9090
SANDBOX_RUNTIME_TYPE=kubernetes
SANDBOX_SECURITY_API_KEY=your-api-key
SANDBOX_STORAGE_STATE_REDIS_ADDR=redis:6379
SANDBOX_STORAGE_FILESYSTEM_PROVIDER=s3
SANDBOX_STORAGE_FILESYSTEM_BUCKET=my-bucket
SANDBOX_POOL_MIN_SIZE=3
SANDBOX_POOL_MAX_SIZE=20
SANDBOX_IMAGES_SANDBOX=sandbox:latest
```

主要配置项：

| 配置项 | 默认值 | 说明 |
|--------|--------|------|
| `runtime.type` | `docker` | 运行时后端（`docker` / `kubernetes`） |
| `pool.min_size` | `3` | 预热容器池最小数量 |
| `pool.max_size` | `20` | 容器池最大数量 |
| `images.sandbox` | `sandbox:latest` | 统一沙箱镜像 |
| `images.gateway` | `sandbox-gateway:latest` | 网络网关镜像 |
| `security.exec_timeout_seconds` | `30` | 单次执行超时时间 |
| `security.sandbox_timeout_seconds` | `3600` | 沙箱最大生命周期 |
| `security.max_memory` | `256Mi` | 沙箱内存限制 |
| `security.max_pids` | `100` | 沙箱最大进程数 |
| `security.network_enabled` | `false` | 是否允许网络访问 |
| `storage.state.redis.addr` | `localhost:6379` | Redis 地址（会话持久化） |
| `storage.filesystem.provider` | `local` | 文件存储后端（`local`/`s3`/`cos`/`oss`/`obs`/`minio`） |
| `storage.filesystem.local_path` | `/tmp/sandbox-storage` | 本地存储目录 |
| `storage.filesystem.bucket` | | 云存储 Bucket 名称 |
| `storage.filesystem.sub_path` | | Bucket 内前缀路径 |
| `workspace.auto_sync_interval_seconds` | `0`（禁用） | 工作空间自动同步间隔（秒），0 表示禁用 |

## 架构

```
┌─────────────┐     ┌──────────────┐     ┌─────────────────────┐
│  HTTP API   │────▶│   Manager    │────▶│   Runtime (Docker)  │
│  (Gin)      │     │  + Pool      │     │   Runtime (K8s)     │
└─────────────┘     └──────────────┘     └─────────────────────┘
                           │
                    ┌──────┴──────┐
                    │   Storage   │
                    │ Redis State │
                    │ + ScopedFS  │
                    │ (Local/S3/  │
                    │  COS/OSS/   │
                    │  OBS/MinIO) │
                    └─────────────┘
```

- **Manager** — 管理沙箱生命周期，维护预热容器池，启动时清理孤儿容器，管理工作空间挂载/卸载/增量同步
- **Runtime** — 抽象层，支持 Docker 和 Kubernetes 两种后端
- **SessionStore** — 基于 Redis 的会话持久化，persistent 模式沙箱在 API 重启后自动恢复
- **ScopedFS** — 限定根目录的文件系统，防止路径逃逸，支持工作目录切换
- **Docker 网络隔离** — 通过 Gateway Sidecar + iptables 实现出站流量过滤
- **Kubernetes 网络隔离** — 通过原生 NetworkPolicy 实现出站流量控制

## 工作空间

工作空间允许将持久化存储路径挂载到沙箱中，实现跨沙箱的文件持久化。

### 同步机制

- **挂载时**（storage → container）：全量打包为单个 tar 归档上传，一次 API 调用完成
- **同步回存储**（container → storage）：增量同步，通过对比容器文件修改时间与上次同步时间戳，仅写入变更文件，删除已移除文件
- 挂载即视为首次全量同步，后续所有 `from_container` 同步均为增量

### 工作流

```
1. 创建 sandbox（可选指定 workspace_path）→ 存储文件自动同步到容器 /workspace
2. 在 sandbox 中执行代码，操作 /workspace 下的文件
3. 需要时手动 sync（from_container）增量保存进度
4. 销毁 sandbox → 自动增量同步回存储，存储路径保留
5. 下次创建新 sandbox，挂载同一路径 → 继续工作
```

### 路径关系

```
存储后端根目录
└── sub_path (配置级，如 "workspaces")
    └── workspace_path (API 级，如 "user123/project-a")  ← ScopedFS 根目录
        ├── main.py
        └── data/
            └── input.csv
```

所有文件操作通过 ScopedFS 限定在 `workspace_path` 目录内，无法通过 `../` 等方式逃逸。

## License

MIT
