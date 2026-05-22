# 分片上传设计文档

**日期：** 2026-05-21  
**状态：** 已确认

## 背景

现有 `POST /sandboxes/:id/files/upload` 接口受 64MB 请求体限制，无法上传大文件。需要新增独立的分片上传接口，不影响现有上传逻辑。分片在容器内 `/tmp/.uploads/` 暂存，上传状态持久化到 Redis，sync 机制天然不涉及 `/tmp`，无需额外过滤。

## 接口设计

### 五个新接口

| 方法 | 路径 | 说明 |
|------|------|------|
| `POST` | `/api/v1/sandboxes/:id/files/upload/init` | 初始化分片上传，返回 upload_id |
| `POST` | `/api/v1/sandboxes/:id/files/upload/chunk` | 上传单个分片（顺序） |
| `GET` | `/api/v1/sandboxes/:id/files/upload/status` | 查询上传状态 |
| `POST` | `/api/v1/sandboxes/:id/files/upload/complete` | 合并所有分片到目标路径 |
| `DELETE` | `/api/v1/sandboxes/:id/files/upload/cancel` | 取消并清理分片和 Redis 状态 |

### Init — 初始化

**请求体（JSON）：**
```json
{
  "path": "/workspace/large.bin",
  "total_chunks": 10
}
```

**行为：**
1. 校验 `path`（validateSandboxPath）
2. 生成 UUID 作为 `upload_id`
3. 在容器内执行 `mkdir -p /tmp/.uploads/{upload_id}`
4. 写入 Redis，key = `sandbox:multipart:{sandbox_id}:{upload_id}`，TTL 24h
5. 返回 `upload_id`

**响应：**
```json
{ "upload_id": "uuid" }
```

### Chunk — 上传分片

**请求：** multipart/form-data，字段：
- `upload_id`（string）
- `chunk_index`（int，0-based）
- `file`（binary）

**行为：**
1. 从 Redis 读取上传状态，校验 sandbox_id 匹配
2. 强制顺序：`chunk_index` 必须等于 `len(received_chunks)`
3. 将分片写入容器 `/tmp/.uploads/{upload_id}/{chunk_index}`
4. 更新 Redis `received_chunks`，刷新 TTL

**响应：**
```json
{ "received": 3, "total": 10 }
```

### Status — 查询状态

**请求：** query param `upload_id`

**响应：**
```json
{
  "upload_id": "uuid",
  "dest_path": "/workspace/large.bin",
  "total_chunks": 10,
  "received_chunks": 3,
  "created_at": "2026-05-21T10:00:00Z"
}
```

### Complete — 合并

**请求体（JSON）：**
```json
{ "upload_id": "uuid" }
```

**行为：**
1. 从 Redis 读取状态，校验 `received_chunks == total_chunks`
2. 在容器内执行 `cat /tmp/.uploads/{upload_id}/0 /tmp/.uploads/{upload_id}/1 ... > {dest_path}`
3. 删除 `/tmp/.uploads/{upload_id}/`
4. 删除 Redis key
5. 返回目标路径和文件大小（通过 stat 获取）

**响应：**
```json
{ "path": "/workspace/large.bin", "size": 104857600 }
```

### Cancel — 取消

**请求：** query param `upload_id`

**行为：**
1. 从 Redis 读取状态，校验 sandbox_id 匹配
2. 删除容器内 `/tmp/.uploads/{upload_id}/`（rm -rf）
3. 删除 Redis key
4. 返回 204

## Redis 数据结构

**Key：** `sandbox:multipart:{sandbox_id}:{upload_id}`  
**TTL：** 24 小时  
**Value（JSON）：**

```json
{
  "upload_id": "uuid",
  "sandbox_id": "xxx",
  "dest_path": "/workspace/large.bin",
  "total_chunks": 10,
  "received_chunks": 3,
  "created_at": "2026-05-21T10:00:00Z"
}
```

## 分层职责

| 层 | 职责 |
|----|------|
| `pkg/types/file.go` | 新增请求/响应类型 |
| `internal/api/handler/file.go` | 参数校验、路径验证、HTTP 响应，新增 5 个 handler |
| `internal/sandbox/manager.go` | 业务逻辑：Redis 状态读写、调用 runtime 操作容器 |
| `internal/api/router.go` | 注册 5 条新路由 |
| Redis（复用现有连接） | 持久化分片上传状态 |

## Sync 隔离说明

- `syncFromContainer` 的 `containerFileManifest` 执行 `find /workspace ...`，不扫描 `/tmp`，分片目录 `/tmp/.uploads/` 天然隔离。
- `syncToContainer` 操作 workspace ScopedFS，同样不涉及 `/tmp`。
- **无需任何额外的 sync 过滤逻辑。**

## 错误处理

| 场景 | HTTP 状态 |
|------|-----------|
| upload_id 不存在或已过期 | 404 |
| chunk_index 不连续（乱序） | 400 |
| complete 时分片未全部到达 | 400 |
| 容器内操作失败 | 500 |
| sandbox 不存在 | 404 |

## 不在范围内

- 分片重传覆盖（顺序强制，不支持重传）
- 并发上传同一 upload_id 的多个分片
- 分片大小校验（由客户端保证）
