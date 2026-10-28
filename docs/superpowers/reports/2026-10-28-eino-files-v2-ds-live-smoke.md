# Eino filesystem × Files v2：DS 实际集群验证（2026-10-28）

## 隔离范围

- 集群：`ds-ai-research`；命名空间：`sandbox-fuse-etcd`。
- 通过临时 Deployment `eino-files-v2-smoke-20261028` 访问已验证的 Files v2 API 镜像 digest `sha256:cd2c32345ae7ae86fedd6a4f2f9d54297dbfc1621aa27ae6f45ddeb0c2de146e`；FUSE runtime 使用 digest `sha256:11a2cad236bc18ea6b146b838b05e640b8db3c3171352364df2b33b2fdd056bc`。
- 临时 API 复用本集群的 Kubernetes、etcd 和 MinIO 连接，状态 scope 为 `sandbox-fuse-etcd/eino-files-v2-20261028`，etcd 前缀为 `/sandbox/ds-eino-files-v2-20261028/`，普通和 FUSE 预热池最小值均为 0。它的 Pod 标签不匹配业务 `sandbox-api` Service；调用经本地 `18080:8080` port-forward。
- 业务 `sandbox-api` 镜像未改，测试后仍有 2/2 Ready 和 2 个 Service 后端。未使用 ChatModel；测试直接调用 Eino middleware 注册的工具。

## 实际结果

`go test -run '^TestLiveEinoFilesystem$' -v -count=1 -timeout 9m` 对真实 DS 临时 API **PASS（10.61 秒）**。新建 FUSE 沙盒 `sandbox-s2a0mydmtc`，独立 workspace `eino-files-live/1793172747176531000`：

| Eino 工具 | 观察到的结果 |
| --- | --- |
| `write_file` | `/eino-demo.txt` 写入 `alpha\nworld\n`，返回 `Updated file /eino-demo.txt`。 |
| `read_file` | 逐行读到 `alpha`、`world`。 |
| `edit_file` | 精确替换 `world` 为 `etcd`，返回下面的 unified diff。 |
| 再次 `read_file` | 逐行读到 `alpha`、`etcd`。 |
| `ls`、`glob`、`grep` | 均返回 `/eino-demo.txt`；`grep` 报告匹配 1 个文件。 |

diff 的末尾空白上下文行在下方省略：

```diff
--- /eino-demo.txt
+++ /eino-demo.txt
@@ -1,3 +1,3 @@
 alpha
-world
+etcd
```

`running.Close` 的公开 DELETE 成功；随后该沙盒 Pod 为 NotFound，成功和首次失败的两个 workspace 的公开 lookup 都为 404。port-forward 已停止，临时 Deployment 和 ConfigMap 已删除，临时 Pod 不存在。对唯一测试 MinIO 前缀 `workspaces/eino-files-live/1793172747176531000/` 先精确列出 0 字节目录标记和 11 字节测试文件，再删除该前缀并确认列表为空；首次失败的 workspace 前缀本来为空。独立 etcd 测试前缀保留，不触碰业务状态前缀。

## 首轮故障与边界

1. 首次创建前，临时配置误填了 63 位 runtime digest，配置校验直接拒绝；没有创建沙盒。
2. 修正后，runtime 镜像在 `ds-ai-worker-1` 首次拉取用了 58.8 秒，超过本次准备路径的有效期限，创建返回 503；准备 Pod 被回收，workspace lookup 最终为 404。
3. 为复用节点缓存而给临时 API 设置单节点 selector，违反了 AppArmor loader 与 runtime selector 必须完全一致的安全合同；临时 API 启动门超时。撤销 selector 后恢复 Ready，未修改 loader 或业务 API。
4. 最终重试使用该节点已缓存的 runtime 镜像，完整 Eino 工具链通过。此结果证明本次真实 FUSE 沙盒上的 Eino 文件工具和 diff 展示，不代表 ChatModel 决策、原子 diff、并发编辑或大文件性能验收。

可重复的 opt-in 测试入口是 `examples/eino-files-backend/live_test.go`。测试只从环境变量读取 API 地址和密钥；输出未包含密钥。
