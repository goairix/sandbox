# etcd 文件 API 与独立探针设计

这是已批准完整迁移的下一功能单元：在显式 `--serve-etcd` 模式提供真实文件列表、递归列表、glob、读取、按行读取、字符串编辑、按行编辑和受限下载；所有文件操作通过原 `PlainColdService.Exec`，保留原签名、fencing、执行身份、续期与物理 Wait/IO join。默认 legacy 入口和大文件上传尚不在此单元切换。

## 方案与边界

不复用旧 Kubernetes raw SPDY 文件操作，不复制旧 Manager，不增加 etcd 文件内容或每沙盒常驻 map。选择在现有 pinned `sandbox-launcher` 增加无特权 `file-operation` 子命令，执行 Go 文件工具；比依赖 Python/find/sed 或新增未部署二进制稳定。它只是原 Exec 的普通非特权 child，不读取控制凭证、不建立新的授权或管理监听器。Argv 固定 `/usr/local/bin/sandbox-launcher file-operation`；操作参数为有限 canonical JSON stdin，完整描述符仍绑定其长度和 SHA。

文件工具独立包 `internal/sandboxfiles`，提供 `Request`、`Result`、`Validate`、`Run` 及有限 stdin/stdout 的 `Serve`。Root self-review 时允许在预检冻结具体签名，不能削弱下列行为。生产只允许 `/workspace`、`/tmp` 及其子路径；固定根由工具内部选择，不能由请求指定 host root。以 Go1.25 `os.Root` 约束实际路径，不仅 lexical 检查；允许根内相对 symlink，拒绝绝对/逃逸链接。普通文件/目录以 nonblocking open 后检查实际 fd 类型，FIFO/socket/device 不作为文件读取。该工具不声称隔离 sandbox 内 bind mount 或并发用户的全部文件系统权限。

## 有限资源与文件语义

- helper JSON stdin 最多1MiB；文件内容读取及编辑前后最多512KiB；helper JSON stdout最多1MiB、stderr64KiB。API沿用2MiB body及≤32活跃业务请求。超限明确413或有限错误，不截断成功。
- 路径最长4096 UTF-8字节，无NUL、绝对且规范化后属于上述根；拒绝原路径 `..` 组件，不误拒合法含双点文件名。glob pattern最长4096，合法相对 `path.Match` pattern，对相对路径匹配；不允许逃逸。分页默认1/100，page_size上限1000，非法负数拒绝；max_depth0表示递归至工具硬界限64，正数≤64。
- 一次目录扫描总条目最多10000、深度64；逐批/单项读取目录，不能 ReadDir(-1) 先无界分配。排序及分页仅在上述有限集合内，返回精确 total_count；超限返回错误，不声称完整结果。非递归只直接子项；递归/glob不跟随目录 symlink，文件名含换行/制表符正确 JSON 编码。
- `files/read` 返回与旧入口相同 text/plain原始内容和X-File-Path；download返回原始bytes、有限安全Content-Disposition；两者均在原 signed terminal 成功后才写成功响应。二进制不会因为JSON字符串转换丢失，helper结果以[]byte编码，API解码后输出原字节。
- read-lines计数包括非空末尾无换行的最后一行；保留空白行；默认start1/end文件结尾，负数/逆序拒绝，越过EOF返回空且保留明确索引。edit替换首个或全部匹配；old_str必须非空，无匹配返回409。edit-lines替换闭区间，end0到EOF；new_content原样拼入前后文本，不隐式补换行；开始超过EOF+1拒绝，EOF+1且end0允许追加。行为差异写operator文档，不能复刻旧 wc/sed 多次执行的竞态。
- 编辑在一次原Exec中完成：实际读取、有限变换、同一固定父目录创建独占随机临时文件、保留原permission低9位、写全/Sync/Close后原子Rename，再Sync父目录。失败清理已创建的临时文件；不截断原目标，不跨 /tmp 与workspace移动。rename以后失败不能当作“未修改”；所有未知回执不自动重发。并发用户编辑仍可last-writer-wins，不声称文件事务或持久幂等。

## HTTP 接入与错误

沿用旧八条路由/DTO：GET files/list、files/download；POST files/list-recursive、glob、read、read-lines、edit、edit-lines。原 UUID校验、单APIkey、严格JSON、全局business bucket保持。每次文件执行拒绝 Idempotency-Key（包括空header）；X-Request-ID有限校验/生成并回传，不能充当去重或权限。固定UID/GID/base env/workdir来自mounted配置，timeout使用已有default/max，RequiresNetwork=false，不继承API环境。

只有原 `PlainExecLocalTerminal`、verified receipt/drain且实际exit0，才解码严格有限helper结果并给成功；helper明确应用错误可在同样verified terminal下映射400/404/409/413/422/500，sanitized messages，不将任意stderr送给用户。原pre-effect rejected使用已验收service映射；实际unknown保留原operation/effect/reason/no-retry，返回502。输出超限取消后实际join原Exec，不能因可读JSON/exit片段推断成功。每次仅一个原Exec，无额外Lookup、raw fallback或自动重试。上传/分片仍typed501，后续独立有限传输单元承接，不能把1MiB stdin扩大解决大文件。

## 独立探针预算

修复前一单元记录的业务占满使health503问题：`GET /health`采用独立有限4槽，`GET /ready`采用独立有限2槽并保留2秒ctx和原Service.Ready；business维持配置1..32槽，互不借用。三个lane共用原closed锁、取消登记和WG join，Close之后均503；无新增goroutine、resident/IP map或无界队列。health不会被ready卡住，且不调用service或消费业务token；ready真实不可用仍503，不能伪造健康。Service自身接纳已满可能使ready503，这是实际可服务性；本单元不更改service总预算。

## 验收

tests-first：真实临时目录的二进制/特殊名/相对symlink及逃逸/FIFO/超限/分页/lines/edit原子发布与permission；真实socket验证descriptor、零副作用拒绝、typedunknown与输出超限join、原HTTP DTO、满business/ready时health仍200及Close等待。补一个新的 Linux actual native selector，通过原TLS-etcd、正式launcher与原service实际写入/列表/读取/编辑/下载，并验证非特权helper/真实terminal与原Wait；只测新文件场景，不重放已验收Lookup/32秒Exec/整HTTP矩阵。

唯一fresh author/reviewer，小步源码提交，Root唯一Go/Git/Docker，全部源码/测试/docs/格式通过apply_patch。不访问已关闭OWN/pair，不碰无关Sentinel文档。受控native不是实际kubelet/生产部署或大量已有sandbox容量验收。后续大文件传输、其他API/Pool/Docker、分区/FUSE、容量、部署恢复和Redis移除继续执行。
