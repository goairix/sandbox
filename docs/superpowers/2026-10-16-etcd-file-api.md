# etcd 模式的有限文件 API

显式 `--serve-etcd` 模式支持九个文件入口，所有请求使用 mounted API key。每次有限文件请求只调用一次原 `PlainColdService.Exec`，通过固定 `/usr/local/bin/sandbox-launcher file-operation` 普通 child 执行。必须更新 sandbox 镜像中的 pinned launcher；旧镜像没有对应操作时会明确失败，不回退 Kubernetes raw exec。child 使用 mounted UID/GID、基础环境、`/workspace` 工作目录和默认执行超时，不读取管理凭证或继承 API 进程环境。

| 方法 | `/api/v1/sandboxes/:id/` 下的路由 | 输入及结果 |
| --- | --- | --- |
| GET | `files/list` | query `path` 默认 `/workspace`；原 `files/path` DTO |
| POST | `files/list-recursive` | 原 `path/max_depth/page/page_size` DTO及精确总数 |
| POST | `files/glob` | 原 `path/pattern/page/page_size` DTO及精确总数 |
| POST | `files/read` | 原 `path` DTO；原始 bytes、text/plain、X-File-Path |
| POST | `files/read-lines` | 原行范围 DTO及明确行索引 |
| POST | `files/edit` | 原字符串替换 DTO；`{"message":"ok"}` |
| POST | `files/edit-lines` | 原闭区间替换 DTO；`{"message":"ok"}` |
| GET | `files/download` | 必填 query `path`；原始 bytes、安全 attachment文件名 |
| POST | `files/upload` | multipart 单个 `file`；成功返回原 `path/size` DTO，文件最多512KiB |

只接受规范化后位于 `/workspace` 或 `/tmp` 的绝对 UTF-8 路径，最多4096字节，拒绝NUL和原路径的 `..` 组件；合法含双点文件名不受影响。Go1.25 `os.Root` 约束实际路径；读取允许根内相对 symlink，拒绝绝对或逃逸 symlink。编辑固定实际父目录FD并拒绝最终目标为 symlink。目录遍历不跟随子目录 symlink，拒绝 FIFO/socket/device作为普通文件。非法UTF-8文件名及 read-lines的非法UTF-8内容返回422，原始read/download保留任意二进制。

合法文件名可以含换行或制表符，列表通过JSON保留它们。read路径含控制字符时，`X-File-Path` 使用百分号编码，并返回 `X-File-Path-Encoding: percent-encoded`；客户端按该标记解码。普通路径header保持原值。download使用安全的 MIME attachment disposition，包括HTML文件。

HTTP JSON body最多2MiB，helper规范JSON stdin最多1MiB；单文件读取和编辑前后最多512KiB。helper stdout JSON最多1MiB、stderr最多64KiB，编码扩张也计入stdout限制；没有截断成功。JSON严格拒绝未知/重复/大小写别名字段、null和尾随JSON。GET查询仅接受单个path，拒绝非空body。分页0默认page1/page_size100，page_size最多1000，负数或超限拒绝。扫描最多10000项，递归硬深度64，显式max_depth为1..64；0递归至硬界限。超过扫描上限返回413，不报告不完整的total_count。glob使用合法相对 `path.Match` 模式匹配相对路径，不提供额外的`**`递归通配语义。

read-lines保留空白行，计入末尾无换行的最后一行，末尾LF不额外产生一行。start0默认为1，end0表示EOF；负数和逆序拒绝。start超过EOF返回空数组，保留请求start及实际end，空文件返回start1/end0/total0。edit要求非空old_str，替换首个或全部匹配，无匹配返回409。edit-lines替换闭区间，new_content原样拼入前后内容，不补换行；start=EOF+1且end0允许追加，更远的start拒绝。这些语义避免旧入口多次wc/sed执行之间的竞态。

编辑将结果写入同一固定父目录中的独占随机临时文件，保留原permission低9位，写全、Sync、Close后Rename，再Sync父目录；失败清理临时文件，不截断原目标。rename后的错误返回 `publish_uncertain`，说明修改可能已发布且不得自动重试。并发用户编辑仍可能last-writer-wins，不提供文件事务或持久幂等；该工具也不额外隔离sandbox内bind mount或用户原有文件权限。

有限上传接受普通 multipart 的前置或尾部 `path` 字段；单个表单路径覆盖 query 路径，缺省 `/workspace/`，尾斜杠拼接文件名。带 `X-Sandbox-File-Size` 时必须提供 query 路径，拒绝任何表单路径，并核对实际文件长度；空文件合法。第二个文件、重复路径、未知字段、截断结尾、声明不符在执行前拒绝。文件上限512KiB，总请求体上限576KiB，包含头部、边界和合法epilogue；读到真实HTTP body EOF后才进入Exec，超限返回413。上传不落API临时盘，不建立上传状态库。

上传逐级通过固定目录FD创建父目录，拒绝父目录symlink及最终目录、symlink、FIFO等特殊目标。新建和覆盖都使用0644的新inode，不读取旧文件，不保留旧权限；复用同目录临时写入、Sync、Rename及父目录Sync流程。发布后的同步失败同样返回 `publish_uncertain`，不得自动重试。512KiB二进制经base64编码仍落在原1MiB helper stdin限制内，未扩大签名执行的输入边界。

所有文件执行拒绝任何 `Idempotency-Key` header，包括空值。`X-Request-ID`最多128个可打印ASCII字节，缺省生成并返回，仅用于归因。只有原签名terminal、drain确认且exit0后才消费helper结果；应用错误映射400/404/409/413/422/500并返回清理后的消息。未知执行保留operation/effect/reason，返回502和不得自动重试提示。输出超限会取消后等待原Exec的Wait/续期/IO实际join。不能因为已看到可读JSON就推断成功。

业务活跃请求保留配置的1..32槽。`GET /health`独立4槽且不调用service；`GET /ready`独立2槽，沿用2秒context及原Service.Ready。三个lane共用关闭、取消登记和WG join，总owner不超过38。业务或ready占满时health仍可用；ready真实失败仍503，service自身容量占满也可能使ready503。Close后全部lane返回503。

分片上传仍返回typed501；普通上传超过512KiB明确返回413，大文件上传仍待实现。Linux验证入口为 `SANDBOX_FILE_API_NATIVE=1` / `TestFileAPINative`，使用原TLS-etcd、正式launcher及原native fixture，验证真实文件、非特权child、签名terminal/drain和原bridge/Wait/IO清理；新增二进制创建、512KiB覆盖及下载比对。独立Linux UID1000测试实际覆盖父目录、特殊目标和发布后Sync失败；这不代表实际kubelet、生产部署或现有sandbox大规模容量已验收。
