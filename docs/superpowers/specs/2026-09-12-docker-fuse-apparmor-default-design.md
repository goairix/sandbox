# Docker FUSE AppArmor 默认行为修复设计

## 问题

Docker 主机启用 AppArmor 时，未显式指定 profile 的容器会使用
`docker-default`。该 profile 禁止 `mount`，因此具备 `/dev/fuse` 和
`CAP_SYS_ADMIN` 的 `sandbox-fuse-docker` 仍无法启动 s3fs。当前文档把
“未配置自定义 profile”错误地等同于“不受 AppArmor 约束”。

## 决策

- 仅对 Docker FUSE runtime 生效：`FUSE_LSM_PROFILE` 为空时，sandbox-api
  创建容器时显式设置 `apparmor=unconfined`。
- 保留 `no-new-privileges=true`、只读根文件系统、能力白名单、独立网络
  和 system egress 策略。
- 非空 `FUSE_LSM_PROFILE` 继续使用宿主机已加载的 AppArmor/SELinux
  profile；显式填写 `unconfined` 仍视为无效配置，避免配置含义混乱。
- Kubernetes sidecar 的 LSM 处理不变。
- FUSE Pool key 版本升级，确保升级后的 sandbox-api 不领取按旧安全语义
  创建的 prepared runtime。

## 部署影响

本次运行时行为由 sandbox-api 生成的 Docker HostConfig 决定，因此只需
重新构建和部署 `sandbox-api` 镜像。`sandbox-fuse-docker`、
`sandbox-fuse-mounter`、`sandbox-runtime` 和 `sandbox-gateway` 无需重建。

升级时保持 `FUSE_LSM_PROFILE` 未配置，替换 sandbox-api 镜像即可。不得
重启 Docker daemon，也不需要手工删除 Pool 资源；sandbox-api 按新 Pool
key 排空旧池并补充新池。

## 验证

- 单元测试确认空 profile 生成 `no-new-privileges=true` 和
  `apparmor=unconfined`。
- 单元测试确认自定义 profile 行为不变，显式 `unconfined` 仍被拒绝。
- 单元测试确认 Pool key 版本变化会改变 key。
- 运行 Docker runtime、sandbox manager、配置与全量 Go 测试。
- 部署新版 sandbox-api 后，通过 API 验证 FUSE workspace 创建、执行、
  测试文件写入/读取/删除和 sandbox 销毁。
