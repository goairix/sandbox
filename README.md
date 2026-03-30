# Sandbox

安全、API 驱动的代码沙箱执行服务，基于 Go 构建。为运行不受信任的 Python、Node.js 和 Bash 代码提供隔离环境，支持细粒度的资源控制与网络隔离。

## 特性

- **多语言支持** — 开箱即用支持 Python、Node.js、Bash
- **双运行时后端** — Docker（通过 Gateway Sidecar 实现网络过滤）和 Kubernetes（通过 NetworkPolicy 实现网络隔离）
- **RESTful API** — 一次性执行、持久化沙箱、同步/流式（SSE）输出
- **预热容器池** — 预热容器池，实现低延迟的沙箱分配
- **安全隔离** — 资源限制（CPU、内存、PID、磁盘）、只读根文件系统、Seccomp 安全配置、网络白名单、API Key 认证、速率限制
- **文件操作** — 沙箱内文件的上传、下载和列表查看
- **对象存储** — 可插拔后端：Local、S3、COS、OBS、OSS
- **Helm Chart** — 生产级 Kubernetes 部署，支持 HPA 自动伸缩

## 快速开始

### Docker Compose

```bash
cd docker
docker-compose up -d
```

服务启动后，API 默认监听 `http://localhost:8080`。

### API 示例

**一次性执行代码：**

```bash
curl -X POST http://localhost:8080/api/v1/execute \
  -H "Authorization: Bearer ***REDACTED_API_KEY***" \
  -H "Content-Type: application/json" \
  -d '{"language":"python","code":"print(\"Hello Sandbox!\")"}'
```

**创建持久化沙箱：**

```bash
# 创建沙箱
curl -X POST http://localhost:8080/api/v1/sandboxes \
  -H "Authorization: Bearer ***REDACTED_API_KEY***" \
  -H "Content-Type: application/json" \
  -d '{"language":"python","mode":"persistent"}'

# 在沙箱中执行代码（使用返回的 id）
curl -X POST http://localhost:8080/api/v1/sandboxes/<id>/exec \
  -H "Authorization: Bearer ***REDACTED_API_KEY***" \
  -H "Content-Type: application/json" \
  -d '{"code":"x = 42\nprint(x)"}'

# 销毁沙箱
curl -X DELETE http://localhost:8080/api/v1/sandboxes/<id> \
  -H "Authorization: Bearer ***REDACTED_API_KEY***"
```

**流式输出（SSE）：**

```bash
curl -X POST http://localhost:8080/api/v1/execute/stream \
  -H "Authorization: Bearer ***REDACTED_API_KEY***" \
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

## 配置

通过 `configs/config.yaml` 或环境变量配置，环境变量以 `SANDBOX_` 为前缀：

```bash
SANDBOX_SERVER_PORT=9090
SANDBOX_RUNTIME_TYPE=kubernetes
SANDBOX_SECURITY_API_KEY=your-api-key
SANDBOX_STORAGE_STATE_REDIS_ADDR=redis:6379
```

主要配置项：

| 配置项 | 默认值 | 说明 |
|--------|--------|------|
| `runtime.type` | `docker` | 运行时后端（`docker` / `kubernetes`） |
| `pool.min_size` | `3` | 预热容器池最小数量 |
| `pool.max_size` | `20` | 容器池最大数量 |
| `security.exec_timeout_seconds` | `30` | 单次执行超时时间 |
| `security.sandbox_timeout_seconds` | `3600` | 沙箱最大生命周期 |
| `security.max_memory` | `256Mi` | 沙箱内存限制 |
| `security.max_pids` | `100` | 沙箱最大进程数 |
| `security.network_enabled` | `false` | 是否允许网络访问 |

## 架构

```
┌─────────────┐     ┌──────────────┐     ┌─────────────────────┐
│  HTTP API   │────▶│   Manager    │────▶│   Runtime (Docker)  │
│  (Gin)      │     │  + Pool      │     │   Runtime (K8s)     │
└─────────────┘     └──────────────┘     └─────────────────────┘
                           │
                    ┌──────┴──────┐
                    │   Storage   │
                    │ Redis + S3  │
                    └─────────────┘
```

- **Manager** — 管理沙箱生命周期，维护预热容器池
- **Runtime** — 抽象层，支持 Docker 和 Kubernetes 两种后端
- **Docker 网络隔离** — 通过 Gateway Sidecar + iptables 实现出站流量过滤
- **Kubernetes 网络隔离** — 通过原生 NetworkPolicy 实现出站流量控制

## License

MIT
