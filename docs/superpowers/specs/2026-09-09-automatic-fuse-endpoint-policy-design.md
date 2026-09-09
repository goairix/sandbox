# Workspace FUSE 自动 Endpoint 策略设计

## 1. 目标

Workspace FUSE 的网络隔离继续由系统强制执行，但运维只配置真实的对象存储信息，不再手工维护 DNS、FQDN、IP、CIDR、端口、凭据目录、CA Secret 或测试开关。

本设计覆盖 Docker 与 Kubernetes runtime，以及 MinIO、华为公有云 OBS、2023 年部署的华为私有云 OBS。一个 release 仍只选择一个后端，sync 与 FUSE 继续共用该后端。

## 2. 最终运维合同

正常部署删除以下环境变量或 values 入口：

1. `WORKSPACE_CREDENTIAL_DIR`
2. `WORKSPACE_SECRET_STAGING_ROOT`
3. `WORKSPACE_SECRET_NAME`
4. `WORKSPACE_CA_SECRET_KEY`
5. `WORKSPACE_MODE`
6. `WORKSPACE_ALLOW_UNVERIFIED_DURABLE_FLUSH`
7. `FUSE_SYSTEM_EGRESS_MODE`
8. `FUSE_DNS_CIDRS`
9. `STORAGE_ENDPOINT_HOST_IPS`
10. `STORAGE_ENDPOINT_FQDNS`
11. `STORAGE_ENDPOINT_CIDRS`
12. `STORAGE_ENDPOINT_PORTS`

前四项属于已经废弃的凭据文件和自定义 CA 路径。当前支持的后端均使用系统信任链中的正常证书；内网无证书 MinIO 使用 HTTP，不需要 CA 文件。第五项由 `WORKSPACE_DEFAULT_MOUNT_MODE` 与 `WORKSPACE_ENABLED_MOUNT_MODES` 取代。第六项是测试门禁，不属于生产配置。其余字段全部改为程序内部派生值。

MinIO 的最小存储配置为：

```dotenv
STORAGE_PRESET=minio
STORAGE_BUCKET=aiadp-workspace-dev
STORAGE_ENDPOINT=minio.example.com
STORAGE_ACCESS_KEY=<access-key>
STORAGE_SECRET_KEY=<secret-key>
STORAGE_REGION=
STORAGE_SUB_PATH=workspaces
STORAGE_USE_SSL=true
STORAGE_IDENTITY=production-minio
WORKSPACE_CREDENTIAL_GENERATION=minio-prod-v1
```

镜像、workspace 模式、Pool 数量与 API 配置仍是独立的普通部署配置。cache、超时和 LSM 等字段保留程序默认值，只在确实需要调优时覆盖。

## 3. Region 派生

`STORAGE_REGION` 变为可选：

- MinIO 为空时使用 S3/MinIO 默认值 `us-east-1`。
- 华为公有云 OBS 从标准 endpoint（例如 `obs.cn-southwest-2.myhuaweicloud.com`）派生 `cn-southwest-2`。
- 华为私有云 OBS 从标准 endpoint（例如 `obs.cn-southwest-268.shuanghuayun.com`）派生 `cn-southwest-268`。
- 运维显式填写 region 时使用该值，但仍执行 canonical 校验。
- 无法从非标准 OBS endpoint 派生时才返回一条明确错误，要求填写 `STORAGE_REGION`。

派生发生在配置规范化阶段，后续 object client、PoolKey 与 mounter bootstrap 使用同一个规范化结果。

## 4. Endpoint 与 TLS

程序从 preset、endpoint 和 `STORAGE_USE_SSL` 生成唯一规范 endpoint：

- MinIO endpoint 继续接受 `host` 或 `host:port`。SSL 开启时默认端口为 443，关闭时默认端口为 80；显式端口优先。
- OBS endpoint 必须是 HTTPS URL，默认端口为 443。
- OBS virtual-host profile 同时派生 endpoint host 与 `bucket.endpoint-host`。
- 所有 HTTPS 后端直接使用系统 CA，不再创建或挂载 workspace CA Secret。

MinIO 允许 `STORAGE_USE_SSL=false`，但仅当本次解析得到的全部地址都是 RFC1918 私网单播地址。任一地址为公网、loopback、link-local、metadata、multicast 或 unspecified 时都 fail closed。华为公有云和私有云 OBS 始终要求 HTTPS。

## 5. 自动 System Egress

System egress 是 FUSE 特权进程的内部隔离机制，与用户 sandbox 的网络开关不同。它继续存在，但不再成为运维配置。

在每次准备 FUSE 空壳时，runtime 使用 sandbox-api 所在环境的系统 resolver 解析规范 endpoint：

1. MinIO path-style 解析 endpoint host。
2. OBS virtual-host 同时解析 endpoint host 与 `bucket.endpoint-host`。
3. 解析结果去重、排序并转换为精确 host CIDR；Docker 当前选择 IPv4 `/32`，Kubernetes 使用集群支持的 `/32` 或 `/128`。
4. 拒绝 unspecified、loopback、link-local、metadata 和 multicast 地址。
5. endpoint 使用显式端口；没有显式端口时从 TLS 状态派生 443 或 80。
6. Docker/Kubernetes 只放行这些精确地址和端口，并通过 `extra_hosts`/`hostAliases` 固定本次空壳使用的解析结果。

FUSE Pod/容器不再需要独立 DNS egress，因此不需要运维提供 DNS CIDR。sandbox-api 自身的正常 DNS 与对象存储访问方式保持不变。

解析得到的 IP 是 runtime 准备结果，不是 release 配置输入，也不写入 API 请求。PoolKey 使用逻辑 endpoint、bucket、端口、TLS 和 credential generation，不使用运维不可控的 DNS 结果。prepared 空壳在 Acquire 前重新核对 endpoint 解析结果；结果变化时销毁旧空壳并按新地址补池，不把旧网络策略交付给请求。

## 6. 用户网络边界

本设计不修改既有用户网络语义：

- 用户请求开放公网时仍默认禁止访问内网地址。
- 用户需要访问其他内网服务时仍必须提交明确白名单。
- 配置中的对象存储 endpoint 是 release 级可信系统依赖，因此其精确解析地址自动加入 system egress。
- 自动放行对象存储不会放行同网段、其他端口或其他内网服务。
- 用户进程仍不能读取 AK/SK、mounter 控制通道或 root-only passwd 文件。

## 7. Runtime 行为

Docker 继续使用专用 gateway 执行 fail-closed iptables 规则。区别是规则输入由 runtime 内部解析生成，而不是来自 `.env`。内网 MinIO 的精确私网 IP 可以进入 system chain；其他内网访问仍受 user chain 规则限制。域名没有可用 IPv4 地址时返回明确的不支持错误。

Kubernetes 统一使用基于解析结果的标准 NetworkPolicy 与 hostAliases，不要求运维选择 `cidr` 或 `cilium-fqdn`，也不要求集群安装 Cilium FQDN policy。动态 Pod 仍不包含 AK/SK。

## 8. 升级与兼容

旧 `.env` 中上述 12 个字段在升级后应删除。程序不再读取这些字段，避免旧值继续影响行为。只修改代码/镜像且 backend identity 不变时，正常执行原有 Compose 或 Helm 升级。

endpoint、bucket、TLS、storage identity、credential generation 或规范化 region 变化时仍触发原有 Pool/backend 排空合同。DNS 结果变化只淘汰尚未绑定的旧 prepared 空壳，不要求运维修改 credential generation。

## 9. 错误处理

启动或准备阶段只报告可以直接行动的错误：

- endpoint 格式非法；
- 非标准 OBS endpoint 无法派生 region；
- 公网 MinIO 关闭 TLS；
- endpoint 没有可用地址；
- endpoint 解析到禁止地址；
- 当前 runtime 不支持 endpoint 的地址族。

不再出现要求填写 `system_egress_cidrs`、`dns_cidrs`、`endpoint_host_ips` 或 `endpoint_ports` 的错误。

## 10. 验证

自动化测试必须覆盖：

- MinIO 空 region 自动成为 `us-east-1`；
- 两种华为 OBS endpoint 自动派生 region；
- 公网 HTTPS MinIO、公网 OBS、私有 OBS 和内网 HTTP MinIO；
- 内网 HTTP MinIO 自动生成精确私网规则；
- 公网 HTTP MinIO 和禁止地址 fail closed；
- Docker 与 Kubernetes 都不要求 12 个旧字段；
- endpoint 解析变化会淘汰旧 prepared 空壳并补池；
- 自动生成规则不扩大现有用户内网访问权限；
- Compose、Helm values、部署文档和示例中不存在已删除字段。

真实环境验收继续分别覆盖 MinIO、华为公有云 OBS、华为私有云 OBS，以及 Docker/Kubernetes 两个 runtime 的 sync 与 FUSE 路径。
