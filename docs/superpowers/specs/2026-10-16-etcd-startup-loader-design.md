# etcd 启动装配与部署检查

本任务落实已批准总迁移方案的生产配置接线。当前库已可完成普通冷建，但cmd/sandbox仍只有Redis路径；先提供实际可执行、只读的部署检查及后续生产入口可复用的装配资源，避免用不完整HTTP能力替代旧服务。

## 契约

- 新增 `sandbox --check-etcd-startup <manifest.json>`，与原drain/audit/recovery/deployment特殊模式互斥，在config.Load、telemetry、Redis与旧Manager初始化前分流。检查成功才退出0；错误退出非0，不监听HTTP、不回退Redis。
- 独立internal/provisioning/deployment包装配现有state.New、SignedClockClient、NewPlainColdService。返回拥有Backend与Service的资源，Close幂等，先关闭/join Service再Backend；构造部分失败回收已建立资源。不得复制原构造验证逻辑或暴露密钥到日志。
- 版本1严格JSON manifest，最大64KiB，拒绝unknown/trailing/duplicate字段及非法UTF8。包含etcd endpoints/identity、TLS CA/client证书/私钥文件、超时/cache预算、固定preparation quota配置、launcher配置文件、root/issuer私钥及issuer证书文件、受控签名时钟socket/公钥/audience、Kubernetes kubeconfig/namespace、固定镜像/nodeName/workerID/resources/principal/defaults。具体DTO与路径由同Task只读预检列明、Root冻结后实施。
- 所有身份、scope/cell、信任root、node UID/boot、contract与issuer必须沿现有构造器验证，不能自动生成或覆盖。node UID/boot等已有launcher字段可直接从严格launcher配置导出，避免重复来源。签名时钟明确固定socket与公钥，无本地墙钟fallback。生产etcd要求HTTPS/mTLS，不开放insecure opt-in。
- manifest只引用挂载的私钥文件，不承载私钥字节；Ed25519私钥使用PKCS8 PEM并校验匹配公钥，临时原始私钥缓冲尽可能clear。TLS文件各≤64KiB，launcher≤16384B，issuer证书≤8192B，签名私钥≤4096B。受控读取必须拒绝目录/管道等非普通文件；允许Kubernetes投影Secret符号链接，避免无界读取。kubeconfig采用现有client-go语义，预检明确external exec/auth plugin处理和输入预算，不执行未显式允许的外部命令。
- 检查只消费已有identity/restore/inventory。禁止InitializePreparationInventory、Put/Delete、LeaseGrant、Pod POST、issuer/clock/identity隐式生成；constructor里的读验证和时钟exchange允许。
- 不修改默认生产路径、Redis数据/依赖、Sentinel文档；本任务不宣称HTTP切换、FUSE/调度或完整迁移完成。

## 预检冻结

Root接受task只读预检的完整DTO：version=1；etcd包含endpoints、显式identity（另含restore_epoch）、TLS文件/server_name、duration字符串和cache预算；preparation包含恰好256个quota_slots与quota_partition；launcher_config_file及root/issuer私钥、issuer_certificate_file；kubernetes包含kubeconfig_file/namespace/max_active/request_timeout；image/node_name/worker_id/resources/business.principal/defaults。manifest路径相对其目录，kubeconfig引用相对kubeconfig目录，无环境或默认路径搜索。

TrustBinding/Roots/ContractDigest/node UID/boot/clock socket、key、audience全部从同一严格launcher配置导出；不新增重复manifest字段。etcd TLS最低1.3，Kubernetes要求HTTPS并沿原staticAPIConfig验证。kubeconfig本身≤64KiB、选中CA/cert/key文件各≤64KiB、token文件≤16KiB，先拒绝所有用户exec/auth插件，再有界内联选中上下文引用并清空路径，之后才调用非交互ClientConfig；不绕过原静态客户端校验。受控打开采用nonblock和实际descriptor普通文件检查，允许投影链接。

导出deployment.Open(ctx,path) (*Deployment,error)，Backend()/Service()只借用，Deployment.Close拥有关闭顺序。检查命令总预算30秒；显式空路径、位置参数、config和所有显式旧特殊模式flag均在任何动作前拒绝。实际Open/Close完成后main才输出固定成功/失败信息。真实集成不必启动launcher：只验证构造和清理，避免无关运行时矩阵。

## 验收

实际有限文件加载/严格解析、错误和秘密不泄漏、模式互斥与早分流测试；host GREEN/race及Linuxarm64相关vet。真实TLS etcd与真实签名时钟集成验证构造成功/关闭、identity不匹配与inventory未就绪拒绝；检查前后无状态写入、无Lease或Pod创建。可复用原正式nativefixture最小变体，具体Root审查collector/命令后才运行；不得以fake成功代替构造成功。唯一fresh wholeTask reviewer，验证后小步提交。
