# Sentinel 原生 sidecar 命令 Implementation Plan

> REQUIRED SUB-SKILL: test-driven-development；原地执行、不执行集群写入、不构建推送镜像。

**Goal:** 增加可运行的 `redis-bootstrap attestor` 命令，读取本成员种子/公钥/固定namespace状态并启动已核验的只读服务。

新建 `cmd/redis-bootstrap/main.go`、`main_test.go`；API Dockerfile 最终额外编译/复制此命令，默认入口不变；实际Chart接入仍须启动包装器/初始化/故障验收完成。

命令只接受 `attestor` 子命令，无其它模式或任意listen/endpoint/provider参数。flags：data-dir=/data、cluster-file=/bootstrap/cluster.json、registration-file=/bootstrap/registration.json、public-keys-file=/identity-public/public-keys.json、private-seed-file=/identity-private/seed、ordinal=-1（显式0..2）、master-name=sandbox。Redis密码只从 REDIS_PASSWORD 环境取得，不允许password flag。

所有文件路径必须明确绝对非根路径；读取regular文件有界（cluster/registration64KiB、公钥1KiB、seed32bytes），拒绝空/超限/非regular/读或close失败，不回显内容、路径和参数。Kubernetes ConfigMap/Secret projection包含可信..data symlink，因此控制面文件允许projection解析，不能套用PVC禁止symlink规则；mount来源和per-member seed subPath隔离由Chart保证。seed文件必须不含特殊mode，owner可写但group/other不可写、other不可读（接受0600/0440），公钥及状态可world-readable。

解析真实cluster/publickeys/seed，用 ParseMemberPrivateSeed 绑定ordinal公钥，不能把其它ordinalseed/64bytes私钥/base64当种子。Initialized必须存在valid registration，其cluster恰等于cluster.json且keydigest与3pubkeys一致；Pending可暂缺registration（仅IsNotExist），一旦存在同样验证。不创建Secret/ConfigMap、不预约PVC、不设置Initialized、不选主、不转发业务。构造IdentityServerOptions后调用固定RunIdentityServer；单测unexported runner seam可检查真实文件解析结果，不作为服务端集成证明。

main使用signal.NotifyContext(SIGINT/SIGTERM)，取消干净退出；异常只打印恒定类别，不输出flag输入/密码/原始文件错误。--help输出静态usage。extra args/unknown flag拒绝不回显。测试实际temp文件、正常LoadedOptions/错误keys/password/phase/registration/非regular/size/mode/取消，并直接go build验证可执行命令。运行时服务的真实TCP测试由server模块负责。

- [ ] stub与真实文件测试RED→GREEN。
- [ ] 拒绝矩阵、seed挂载mode与错误脱敏；帮助无IO。
- [ ] focused/full/race/vet/lint与规格后质量独立复审；主 agent提交精确文件。不要把命令通过称为内置Sentinel部署完成。

## 执行证据

帮助stub RED→GREEN，再用真实固定成员文件与未实现启动stub观察 RED→GREEN；取消期间服务错误另有真实 RED→GREEN。真实文件拒绝矩阵、Initialized缺登记/换公钥/换cluster、0440可信projection、FIFO非阻塞均通过。独立规格后质量复审通过，reviewer tests/race/vet/build均通过。主agent full/race/vet/lint和默认Helm验证通过；实际CLI已在隔离真实Redis夹具中完成冷inventory/新run_id签名及SIGTERM退出，结果见 `docs/testing/2026-09-13-sentinel-identity-runtime-results.md`。完整Sentinel功能与故障矩阵未完成。
