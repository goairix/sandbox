# etcd 大文件下载接入

已批准完整迁移的下一交付单元。基线 `c8f9a23ca59ff8ea781fb5b709ff968914c9fb31`。已有八条文件API内容上限512KiB；这里把显式配置后的 GET files/download 扩展到最多2GiB，保持原执行权限、原终态确认与有限内存。上传/分片仍未完成，不能把下载范围验收记成完整文件传输。

## 选择

原Exec stdin在两端完整载入，硬上限1MiB；原stdout则按32KiB frame同步交给writer。选择一个原Exec运行普通非特权 `sandbox-launcher file-download`，将binary stdout写入私有有总额度的API磁盘暂存。只有原signed terminal/drain/exit0及暂存完整封存成功，才发送HTTP200和文件内容。这样无需扩大stdin、新增权限协议、逐块新建etcd operation，或在原终态前发出成功header。

不改变小文件read/JSON工具及原执行wire、signature、effect/guard/renewal/Wait。文件内容不写etcd，无额外Lookup/rawexec/自动重试、每sandbox常驻map或背景清理协程。大文件上传必须另作有界流式接入，不能将2GiB变成一个[]byte。

## 可选 mounted 配置

HTTP manifest新增严格可选 `file_transfer` 对象：`directory` 为必须已存在的absolute canonical私有目录；`max_file_bytes` 默认2GiB，范围1..2GiB；`max_total_bytes` 默认4GiB，范围max_file_bytes..16GiB；`max_active` 默认2，范围1..8；`timeout_seconds` 默认300，范围1..HTTP max_exec_timeout_seconds。全对象缺省保留已验收的512KiB下载路径，旧manifest/check入口兼容；提供对象则不能静默忽略非法字段/负值/无效预算或失败回退。

loadHTTPSettings仅解析/验证，保持只读startup check不创建文件或清理目录。Gateway.New打开store，要求目录实际owner为当前API EUID、permission0700、最后组件非symlink、NONBLOCK|DIRECTORY实际FD校验，非阻塞flock保证一个原store拥有目录。所有配置为owned copies，调用者后续修改不能改预算/路径。目录由operator挂载准备，不自动mkdir任意host路径。

## 私有有界暂存

新增窄 `internal/transferspool` 包，仅普通HTTP传输数据，无task/authority概念，不暴露旧taskArchiveStore内部能力。Options/Open、Reserve、Job的Write/Seal/Read/Close、Store.Close确切签名由预检冻结。全部结构只跟踪≤8个活跃或保守保留的job，不按已有sandbox数增长。

Reserve在任何文件创建或原Exec之前检查ctx、closed、max_active和总磁盘额度，并预收整个max_file_bytes，失败立即有限429/503，不排队、不启动原工作。临时文件固定命名 `http-download-` 加32位lowerhex随机值、同一目录FD的O_EXCL|NOFOLLOW创建0600，要求regular/owner/nlink1并保留dev/ino。Write每次≤32KiB工作buffer、总量≤job上限；文件IO或超限失败取消原Exec，等实际原Wait/IO返回才关闭/回收。stdout总量不入内存，stderr独立64KiB。

Seal在原Exec成功之后Sync、核对原文件/目录项身份和实际大小/计数、Seek0，不能将可读部分当完整结果。Read顺序有限buffer；不导出任意路径/裸FD。Close先由原handler停止writer/reader并物理join，再同身份Close/unlink；只有确认成功才归还磁盘额度。异常identity、Close/remove失败保守收费并latch不可用/错误，不能继续超售或吞掉错误。Store.Close先关闭接纳/取消原jobs，再实际等原job owner完成，最后关闭目录FD/锁，错误可见；timeout不冒充join。

API崩溃后暂存只含可丢弃的只读下载数据，不是新的写入权限。新store拿到目录flock后，先逐项读取并验证最多8个旧临时文件（精确命名、owner0600/regular/nlink1/dev/ino，无其他未知项/子目录），再按核对身份删除并Sync目录。未知文件、错误权限、超界或异常identity使启动失败，不能广泛清空目录。测试必须覆盖锁互斥、真实子进程退出后锁释放及有限stale恢复；不把普通Close当crash模拟。

## 普通目标下载子命令

`sandbox-launcher file-download`只接收规范JSON `DownloadRequest{path,max_bytes}` stdin，上限16KiB；根路径/4096字节/UTF-8/原始..组件/相对symlink策略沿已验收工具。固定UID/GID/baseenv/workdir和配置传输timeout来自mounted配置，RequiresNetwork=false。单独的普通文件binary输出方法复用真实Root/NONBLOCK/type检查，私有临时root仅供同包测试，无生产hostroot参数。

实际regular FD先检查size≤max_bytes，再用32KiB buffer输出精确原size并要求EOF；缩短、增长、读取/写入失败都不得正常exit0。空文件合法。应用错误只在stderr写一个小的严格Failure code对象并exit1，CLI不再附加普通错误前缀；stdout永远只包含文件bytes。unknown/malformed/非保留exit值仍用sanitized通用错误。具体typed CLI退出实现由预检冻结，不能改其他已有mode的日志/退出行为。

## HTTP与生命周期

只在配置file_transfer时，将原 GET files/download 的已验证path交给新消费者。复用原auth、UUID、query/body、Idempotency-Key presence拒绝、X-Request-ID与business lane；内部spool额度不足无原Exec效果。每次恰好一个原service.Exec，descriptor绑定固定argv、canonical DownloadRequest、完整profile/timeout。stderr64KiB有限，写失败/超限取消并实际等原返回。

原rejected沿service映射；真正unknown沿原operation/effect/reason/no-retry。只有verified terminal exit0再Seal、发送原始内容、安全MIME attachment、实际Content-Length。signed exit1且严格已知Failure允许400/404/413/422/500；malformed、非零其他结果502，不能相信任意stderr或可读stdout。目标文件变化/超界导致失败则discard暂存，不先写200。

HTTP交付用32KiB loop、ctx检查及真实socket写期限（单次至多10秒且不超过总ctx），失败不能假报完整body，客户端以Content-Length发现截断。断连/服务关闭须实际等待原Exec及所有磁盘/HTTPowner，不能后台继续拷贝。private job完成/清理失败影响后续接纳及ready503，但health仍只表存活。

Gateway.Close改为返回缓存的cleanup error，所有生产Serve/CLI关闭路径接收并向上传播；更新必要原test cleanup包装以适配返回值，不改测试断言。Serve先stop/cancel并物理join，再关store、最后Deployment沿原顺序关闭；已closed的Close幂等返回相同错误。正常调用者不应悄悄丢失磁盘关闭错误。

## 验收

tests-first覆盖真实暂存容量/文件类型/身份/权限、超限/IO失败不退款、原owner join、锁/实际进程crash及stale/unknown文件恢复；配置边界/clone/旧manifest；目标binary大于512KiB、空/超界/FIFO/symlink/原大小变化；真实socket单Exec/receipt前不发200、stderrtyped/unknown、慢客户端/取消/Close与实际清理。只执行受本单元影响的host/race/Linuxvet。

新 `TestLargeDownloadNative` / `SANDBOX_LARGE_DOWNLOAD_NATIVE=1` 复用tracked冷fixture最小variant，在正式launcher/原service上读取实际大于1MiB的文件，经私有spool和HTTP校验实际bytes长度/校验值、控制材料不可读、原receipt/descriptor/drain/Wait及精确容器清理。保留原16bridge界限，具体有限场景数预检冻结；不重放旧Lookup/32秒Exec/全部文件矩阵。受控fixture不是生产kubelet或大N容量验收。

唯一fresh author/reviewer，小步提交，Root唯一Go/Git/Docker；源码/测试/格式/docs仅apply_patch。不读取或复用已关闭OWN/pair，不碰无关Sentinel文档。验收后继续上传/其他API/Pool/Docker、分区/FUSE/容量/部署与Redis移除。
