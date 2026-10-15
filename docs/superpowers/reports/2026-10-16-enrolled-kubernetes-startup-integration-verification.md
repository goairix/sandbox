# Enrolled Kubernetes 启动组合验证

范围：正式 launcher 安全接收默认 MemoryEmptyDir 初始权限，并经生产 Kubernetes WebSocket bridge、正式 bridge ELF 和原管理 TLS 验证 publication。BASE `937f0189fc0c4e323c841bffa68c5c99144415b9`，产品最终提交 `fe7d9819fac7a686d04e470622b1e5e31ff20b00`。唯一完整审查 C0/I1/M0 后，同一评审者限定 R1 为 Approved、I1已解决、直接新增 C0/I0/M0；Root已完整读取并接受该范围。

## 小步提交与实际行为

| 提交 | 结果 |
| --- | --- |
| `ee0bc98` | 原 root FD 接受 exact directory0700/0755/0777/01777，先 chmod0700/Fstat 再枚举子项；root/PID1/祖先、旧文件及 O_EXCL/NOFOLLOW 不放宽。 |
| `1deb965` | TLS CloseWrite 后恢复传输 FIN 的有界写期限；bridge 读期限可替换，空闲或未发送写超时不误杀读取，真实在途写超时仍物理中止并保留首因。 |
| `ddd2cd3` | 新默认 tmpfs、正式 ELF/生产 WS 组合用例；原 Wait/IO/Serve/容器清理保留，脚本所有必需断言显式失败退出。 |
| `fe7d981` | 严格 TLS EOF 后，在原有界上下文及读期限中等待原 raw transport n0/EOF，再成功关闭；不提前关闭尚欠真实 exec status 的连接。 |

## 验证与证据边界

新 mode 回归的0777/01777在修复前实际失败；新原生 RED 使用独立1MiB tmpfs且没有 mode override，内核实际 control041777。正式 CLI 原错误是 invalid fresh control directory，child/attach/container退出1，并完成原服务退出和精确清理。

原首次组合失败实际 attach/container退出1，暴露 TLS close_notify 把底层写期限设为 now 后误杀响应的问题。其旧脚本曾错误返回0/FINISHED；原矛盾及全部失败输出保留，没有计作通过。脚本随后以显式失败分支修复，另有不运行 Docker 的断言回归。新 TCP/TLS、真实生产 WS 定向回归覆盖方向期限、FIN 和原 entered writer。

唯一完整 Task review（`937f018`→`ddd2cd3`）为 C0/I1/M0。I1指出 TLS EOF 不等于 exec 终态，deferred Close 可能先于 bridge Wait、WS Success 与 normal1000。R1 新真实 Destination TCP/TLS gate 的四模式在旧代码均提前返回 nil；修复后 success、额外字节、cancel、deadline 按期通过。新生产 WS/TLS gate 延后真实 status，验证等待期间有限超时、恢复期限后只接受 Success/normal close、拒绝非零 status。该修改不是对 crypto/tls 内部 read-ahead 缓冲的通用扫描。

R1限定 host2/race2/Linux vet2 均原退出0，两个 vet 日志为空。最初 WS race 在链接阶段 ENOSPC、测试未执行；该原失败保留，Root仅清理不属于9842个当前输入路径的旧生成缓存后以新 epoch 验证通过。没有删除源码、验收产物或依赖。

各 epoch 独立按实际17个 GoList 字段重建前后文件集合、SHA及所有 source archive 成员，并核对当前组件输入。最终 R1 native runner9078、launcher2001输入，Go1.25.6/linux-arm64/CGO0；最终16个 Task 源文件与冻结清单一致。较早 CLI 策略和中间修复 epochs 是各阶段证据，不能描述为最终同一版本的全套测试。

## R1 原生运行

精确命令为 `ENROLLED_STARTUP_EPOCH=terminal-green-r1 bash scripts/test-enrolled-kubernetes-startup.sh run`；原退出0，attach/container Wait0，无 OOM，精确容器 `8002737ae8d33dc1fbd762d7575094ecd409c8b36dd653b629f9bbb5ba82fb3d` 已删除，owned inventory为空。

- 脚本 SHA256：`6a4d3aab4599648ae7ab311666586a16eea7e00ebb5faedf02e92e491ee06e22`。
- runner SHA256：`5847a783a375f7243ad5093e668b7bda521a9feb59633ee29e4c996b3cb20f15`。
- launcher SHA256：`55e608229f6f1390bb7880654c310dcd23c058c32a6baa9b85d2b5f86d53f649`。
- 实际初始 control041777、workspace040777、tmp041777；正式 PID1 收紧 control0700/config0600，并验证原只读 socket/clock mounts 和精确 ELF。
- 原 verifier 认证 ready1865B，摘要 `3b9aef9caff26e66220ee194b4a892cf23f999e6c12b5b11d45b61825c30251b`。
- 原 bridge Cmd.Wait0、全部 IO/handler join、relay nil；原 PID1 Cmd.Wait0、实际 cgroup empty；observer/clock/etcd原 owner 收尾，etcd SIGTERM和真实 shutdown warn/error未隐藏。

隔离为固定缓存 runner镜像、512MiB/2CPU/256PID、network none、private cgroup namespace、无 host PID、只读 root、NNP、三个精确 OWN 只读文件挂载。新 fixture 的 mount setns 需要 setup-only SYS_CHROOT；两个固定 helper 在正式 exec 前连同 ADMIN/PTRACE 从 bounding 等集合实际丢弃并核验原四cap，生产权限不增加。

## 未交付范围

这是正式程序与生产传输的隔离组合验证，workspace tmpfs仅为权限类比；没有真实 kubelet/CRI 或 disk EmptyDir 验收。尚未交付业务 Create/Resume、持久 quota/32准备槽、Manager/main 切换、调度、FUSE、Docker、完整容量或 Redis 移除。已有 sandbox 数量 N 的性能和容量结论不由这个少量 fixture 推出。

下一完整 Task 是具体 plain-empty 冷建 Create/Resume：持久准备账本、真实一次 Pod POST、原 Hello/activation/Bind/Mount/Publish 和可恢复结算。总体功能进度暂仍约55%，整体交付约40%，生产切换尚未完成。
