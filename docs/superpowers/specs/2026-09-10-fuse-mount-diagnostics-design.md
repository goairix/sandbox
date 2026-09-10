# FUSE 挂载失败诊断设计

## 背景

线上 Docker 环境中，普通 sandbox 与 sync workspace 均可正常访问同一套 MinIO，但 FUSE workspace 在授权后失败，API 仅返回 `workspace ready status does not match authorization`。当前实现丢弃了 mounter 状态读取错误以及 s3fs 标准错误输出，因此无法区分宿主机 FUSE 权限、DNS、TLS、存储认证、bucket 或其他 s3fs 启动问题。

## 目标

- 在不泄露凭证、endpoint、workspace prefix 等敏感信息的前提下，返回可操作的 FUSE 失败类别。
- 保留 Docker 与 Kubernetes 两种 runtime 中的底层安全错误，不再统一覆盖为 ready 状态不匹配。
- 保持现有 ready 校验强度、网络策略和数据安全语义不变。
- 使用同一套 mounter 镜像为 MinIO、华为公有云 OBS 和私有云 OBS 提供一致诊断。

## 非目标

- 不放宽、跳过或重试接受失败的 ready 校验。
- 不修改“允许公网、禁止内网；访问内网需白名单”的网络规则。
- 不把原始 s3fs stderr、AK/SK、endpoint、bucket 或对象前缀返回给 API 调用者或写入普通日志。
- 不改动用户 workspace 数据。

## 方案

### 1. 有界捕获 s3fs 启动诊断

`CommandRunner.Start` 为 s3fs stderr 使用不会阻塞子进程的有界内存缓冲区，最大保留 16 KiB。进程退出后仅使用缓冲区做本地分类，原始内容不通过控制 socket、runtime 或 API 返回，也不写入日志。

分类结果为固定安全代码：

- `fuse-permission`
- `endpoint-dns`
- `endpoint-tls`
- `storage-auth`
- `storage-bucket`
- `s3fs-exited`

分类无法确定时使用 `s3fs-exited`。可附带退出码，但不得附带原始命令参数或 stderr。

### 2. 可靠传递进程退出结果

Supervisor 为每次 s3fs 启动创建容量为 1 的进程结果通道。reaper 调用 `Wait` 后先写入结果并通知退出，再更新内部状态；`waitForMountLocked` 观察到进程提前退出时读取该结果并生成安全分类错误。该结构避免当前仅关闭 `processDone`、丢弃 `Wait` 错误造成的信息缺失，同时避免锁重入死锁。

授权被替换、卸载或进程正常结束后，相关诊断状态必须清除，不能污染下一次池化复用。

### 3. 控制协议只传递安全错误

mounter 控制 socket 的响应增加固定错误代码字段。服务端将内部错误映射为受控协议代码，客户端将其还原为可识别的安全错误；任意未识别错误统一映射为 `rejected`。协议继续沿用现有消息大小限制。

不在协议中传递任意错误字符串。这样即使 s3fs 输出包含凭证、签名、URL 查询参数或对象路径，也不会穿透到 sandbox-api。

### 4. runtime 保留失败阶段

Docker 与 Kubernetes runtime 在读取 mounter ready 状态失败时，返回带阶段的底层安全错误，例如 `read workspace ready status: storage-auth`，不再覆盖成统一的状态不匹配错误。

若状态读取成功但授权值不一致，则只返回不一致的字段名，例如 `workspace ready status mismatch: state,cache_limit`，不回显字段值、runtime UID、pool key 或 workspace prefix。

### 5. API 行为

现有 HTTP 状态码和 sandbox 创建失败语义保持不变，只提高错误消息的可诊断性。失败的 FUSE runtime 仍按原有清理流程回收，不会作为 ready sandbox 交付。

## 测试方案

实现遵循测试先行：

1. Runner 测试验证 stderr 捕获有界、分类正确，且错误文本不包含模拟凭证和 URL。
2. Supervisor 测试验证 s3fs 提前退出时能得到对应安全类别，并验证后续授权不会继承旧诊断。
3. 控制协议测试验证服务端只发送固定代码、客户端能识别，未知内部错误不会泄露。
4. Docker 与 Kubernetes runtime 测试验证底层安全错误被保留，状态不一致只报告字段名。
5. 现有挂载成功、ready、卸载、池化复用测试保持通过。
6. 执行 Go 全量测试、`go vet`、Helm 渲染、Docker Compose 配置检查和现有 FUSE 合约测试。
7. 重新构建并部署 `sandbox-api`、`sandbox-fuse-mounter`、`sandbox-fuse-docker` 后，在用户指定的线上测试 workspace 验证 FUSE 创建、读写及删除唯一测试文件；不读取或修改既有文件内容。

## 发布与回滚

该改动需要同时更新 `sandbox-api`、`sandbox-fuse-mounter` 和 `sandbox-fuse-docker`。协议变更保持向后可解析：缺失新错误代码时沿用 `rejected`。回滚时三个镜像恢复到上一版本即可，不需要修改 `.env`、Helm values 或存储数据。
