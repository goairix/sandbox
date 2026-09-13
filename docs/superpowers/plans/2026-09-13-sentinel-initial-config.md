# Sentinel 初始配置生成 Implementation Plan

> REQUIRED SUB-SKILL: test-driven-development；原地执行，不访问集群、不提交真实凭据。

**Goal:** 为已经登记的 Pending 三卷组生成四条认证链路完整的初始配置，供可恢复本地事务使用；不把生成器当重复引导许可。

**Architecture:** 纯函数只接受有效登记、公钥集和同 marker 的 Reserved 身份。初始0号为主，其它为从；Configured/Initialized 输入拒绝。两个密码均使用内置 raw-rewrite-safe 校验。固定公钥派生 Sentinel myid，并预置其它两名 Sentinel 的 DNS/myid，以便冷启动发现；后续不可重新覆盖已保留的 myid/epoch。

新建 `internal/redisbootstrap/initial_config.go`、`initial_config_test.go`。

```go
type InitialConfigOptions struct {
    MasterName string
    DataPassword string
    SentinelPassword string
    DownAfterMilliseconds int
    FailoverTimeoutMilliseconds int
    ParallelSyncs int
}
func RenderInitialMemberConfigs(BootstrapRegistration,[3]ed25519.PublicKey,VolumeIdentity,InitialConfigOptions) ([]byte,[]byte,error)
```

masterName 为1..128 ASCII alnum/_/-；downAfter 1000..60000ms；failoverTimeout 为至少两倍 downAfter、至多600000ms；parallelSyncs 1..2。注册 cluster Pending、digest与公钥一致、identity Reserved、cluster/member/marker符合固定 ordinal。失败返回两份 nil 配置与常量错误，不输出密码。

Redis 固定 port6379、bind0.0.0.0、protected-mode yes、daemonize no、stdout日志、dir /data、AOF yes/everysec、禁周期RDB(save空)、min-replicas-to-write1/max-lag10、requirepass/masterauth 数据密码、固定 DNS replica-announce-ip/port。非0号 replicaof成员0；禁止 REPLICAOF NO ONE 命令、不可生成任意角色。

Sentinel 固定port26379、非daemon、stdout日志、dir /data、protected-mode yes、requirepass/sentinel-pass Sentinel密码、auth-pass数据密码、deny-scripts-reconfig yes、resolve/announce-hostnames yes、本地DNS announce、monitor成员0 quorum2、config/current/leader epoch0、myid=SHA256(该ordinal公钥)的前20bytes lowerhex；预置其它两Sentinel DNS/myid和两个非0 Redis known-replica。校验派生myid三项唯一。所有值经 QuoteConfigValue，numeric值可信格式化，不使用shell拼接。

生成后使用已有 ParsePersistentConfigs 校验真实配置语法与固定角色/monitor；纯函数不读写文件、不决定实际卷是否仍 Reserved、不检查当前主、不给重复seed授权。包装器必须另行做已登记身份、本地事务和恢复证据检查。

- [ ] RED：三个成员的真实生成配置可被持久解析器解释，认证/AOF/固定DNS/初始角色正确。
- [ ] GREEN及拒绝矩阵：错误marker/ordinal/keydigest/phase/Configured/password/masterName/预算，错误脱敏。
- [ ] focused/race/vet/lint，规格后质量独立复审；实际重写/重启另需Redis fixture，不能用字符串断言冒充。
