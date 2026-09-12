# Docker 文件上传与网络资源恢复设计

## 问题

线上 Docker + MinIO 回归发现两个独立问题：

1. multipart init 已在容器内创建 `/tmp/.uploads/<upload-id>`，但 chunk
   上传通过 Docker `CopyToContainer` 写入时提示目录不存在。普通 sandbox
   的 `/tmp` 是 tmpfs；容器内进程能看到该目录，Docker daemon 的归档复制
   路径看不到该 tmpfs 内容。
2. 普通 sandbox 动态启用网络时，Docker 报默认 IPv4 address pool 已耗尽。
   当前销毁路径忽略 pair network 清理错误，已有的孤立空网络也没有安全的
   自动回收入口。

## 决策

### 文件上传

- Docker runtime 的 `UploadFile` 对普通 sandbox 和 FUSE sandbox 都通过
  容器内 `tar` 管道写入。
- 保留现有的定长流校验、UID/GID 1000、临时文件、原子 `mv` 发布和失败
  清理语义。
- 不把 multipart staging 迁到 `/workspace`，避免临时 chunk 进入对象存储。
- 不再依赖 Docker `CopyToContainer` 写入 tmpfs、bind mount 或 FUSE mount。

### 网络回收

- 普通 Pool 容器被领取后会改名为逻辑 sandbox ID，但 Docker label 不可变。
  动态建网与销毁都必须使用容器当前名称作为 pair network 身份，不能在销毁
  时回退到旧 pool label，从源头避免遗留仍连接 gateway 的网络。
- 普通 sandbox 销毁后，gateway/pair network 清理错误不再被静默忽略；
  容器删除错误与网络清理错误合并返回。
- Docker runtime 初始化时，仅扫描 sandbox 自己管理的网络：
  `sandbox.managed=true` 且名称为 `sandbox-pair-*` 或历史兼容名称。
- 仅删除同时满足以下条件的网络：创建时间至少五分钟、网络内没有任何
  container endpoint。共享网络、第三方网络、仍有 endpoint 的网络均不动。
- 创建 pair network 遇到 Docker 默认地址池耗尽错误时，执行一次相同的
  空网络回收并只重试一次；其他错误不触发重试。
- 不删除孤立但仍连接 gateway 的网络，避免在并发创建或状态不确定时误删
  容器。此类资源需要后续通过明确的运行时身份恢复或人工审计处理。
- 不修改公网访问、内网禁止、永久禁止 metadata/loopback 等现有 iptables
  策略，也不新增环境变量、Docker daemon 配置或部署步骤。

## 错误处理

- 回收扫描失败时保留原始 Docker 错误；runtime 初始化直接失败，避免在
  资源状态未知时继续启动。
- 地址池耗尽后的回收没有释放网络时，重试仍返回 Docker 原始创建错误。
- 单个符合条件的空网络删除失败时返回带网络名的错误，便于线上定位。
- multipart 上传的 tar/流长度/publish/cleanup 任一步失败，继续沿用现有
  错误返回和临时文件清理规则。

## 验证

- 单元测试先复现普通 sandbox 上传仍调用 `CopyToContainer` 的错误路径，
  再确认修复后普通与 FUSE 上传均使用容器内 tar 且保留所有权与定长校验。
- 单元测试覆盖：只删除超过五分钟的空 managed pair network；保留新建、
  有 endpoint、共享及第三方网络；地址池耗尽后回收并重试一次；销毁时不
  吞掉网络删除错误。
- 运行 Docker runtime 定向测试、`go test ./... -count=1`、`go vet ./...`
  和 `go build ./cmd/sandbox`。
- 部署新版 sandbox-api 后，在现有 Docker + MinIO 环境重新验证 multipart
  init/chunk/status/complete/cancel、普通 sandbox 网络更新及完整 FUSE 链路。
- 测试只使用唯一临时 workspace 路径，完成后删除并再次检查无残留。

## 部署影响

仅 `sandbox-api` 代码发生变化，需要重新构建并更新该镜像。其他 runtime、
FUSE mounter、FUSE Docker 和 gateway 镜像无需重建；`.env` 无需增加配置。
