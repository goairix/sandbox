# etcd HTTP 大文件下载

在已验收的原执行链路上，可选启用最多 2GiB 的普通文件下载。入口仍是 `GET /api/v1/sandboxes/{id}/files/download?path=...`，沿用原认证、请求校验和 `X-Request-ID`；每次恰好运行一个普通 UID/GID 的 `sandbox-launcher file-download`。目标文件内容经原 stdout frame 写入 API 私有磁盘暂存，不写入 etcd。

只有原执行返回已验证的 terminal/drain/exit0，且暂存完成 Sync、身份与长度检查后，才发送 HTTP200。交付设置真实 Content-Length，以 32KiB buffer 读写，并使用不超过10秒、且不超过传输总 deadline 的实际 socket 写期限。响应头之前的交付失败明确返回503；响应头之后的断连或超时可能截断正文，客户端必须检查 Content-Length，不能把部分文件当成完整成功。

## 可选配置

在 mounted manifest 的 `http` 对象中增加：

```json
"file_transfer": {
  "directory": "/var/lib/sandbox/http-downloads",
  "max_file_bytes": 2147483648,
  "max_total_bytes": 4294967296,
  "max_active": 2,
  "timeout_seconds": 300
}
```

目录必须由 operator 预先创建和挂载，使用 API 进程的实际 EUID 所有权、权限0700、absolute canonical 路径，最后一个组件不得为 symlink。示例（替换 `API_UID` 为实际数值）：

```sh
install -d -m 0700 -o API_UID /var/lib/sandbox/http-downloads
```

不要让其他应用共用这个目录。Gateway 启动时检查实际目录 FD，并获取非阻塞独占 flock；同一目录只能由一个原 store 持有。startup check 只验证配置，不创建目录、取锁或清理暂存；实际 serve 打开 store 才执行这些检查。

全对象省略时保留原512KiB下载路径。对象存在则不能回退或忽略错误。`directory` 必填；其余字段省略时分别默认2GiB、4GiB、2、300秒。文件额度1..2GiB，总额度必须至少等于文件额度且不超过16GiB，活跃数1..8，timeout 为1..`http.max_exec_timeout_seconds`。显式0、负值、null、未知字段、重复字段和错误预算均拒绝。若 HTTP 最大执行 timeout 小于300秒，须显式设置相容的传输 timeout。

每个 job 在任何文件创建和原 Exec 之前预收整个 `max_file_bytes`，不按实际已写字节提前退款。容量不足立即429，关闭或故障 store 返回503，不排队。应用错误仅信任已验证 signed exit1 的严格六种 Failure code；原执行 unknown 仍保留原 operation/effect/reason 和禁止自动重试语义。路径和相对 symlink 策略保持原普通文件工具约束；escaping symlink 实际拒绝并返回已签名 `io_error`/500，FIFO 为422、初始超界为413。

## 清理与故障

请求取消与服务关闭会取消原执行，但必须实际等待其 Wait/IO 及 HTTP owner 返回后才关闭暂存文件。清理时核对原 dev/ino、owner、0600、regular、nlink1，只删除原目录 FD 下的同身份文件。IO、额度、身份或清理异常会保守保留收费并锁定后续下载接纳，`/ready` 返回503；`/health` 仍表示进程存活。关闭错误经 Gateway.Close、Serve 和 CLI 向上传播。

API 崩溃后的下载暂存可丢弃。下次取得原目录 flock 后，只检查最多8个严格命名 `http-download-` 加32位小写hex的旧文件；全部身份和权限验证完成后才逐项清理并 Sync。未知文件、子目录、错误权限、额外硬链接、异常身份或超过8项使启动失败，不会广泛清空目录。发生此类故障时先停止拥有该目录的进程并检查具体文件来源，再由 operator 处理。

## 本单元验收范围

新 sole native selector 是 `TestLargeDownloadNative`，开关 `SANDBOX_LARGE_DOWNLOAD_NATIVE=1`。使用原受控冷启动 fixture、正式 launcher 和原 service：三个 Create bridge、一个 seed Exec、五个下载 Exec，总计九个原 bridge，保留原16个上限。实际大文件放在 sandbox `/tmp`，不扩大原 `/workspace`1MiB fixture 挂载；验证实际1MiB+37字节 binary 的长度与 SHA256，以及 empty200、oversize413、FIFO422、escaping-link signed500，包含 UID/GID1000、控制材料不可读、descriptor/receipt/drain 和实际 Cmd.Wait/IO/清理检查。该 fixture 不代表生产 kubelet、分区/FUSE 或大规模容量验收。

下载仍只允许 `/workspace`、`/tmp` 普通文件，stdin 保持原有上限且新 helper 请求另限16KiB。小文件 JSON 工具保持原边界。**大文件上传及分片尚未完成**；本下载单元不能记作完整文件传输迁移，也未扩大原权限、transport 或 stdin 能力。
