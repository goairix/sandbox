# 内置 Sentinel 实施前可行性验证 Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking. 本轮选择同会话内联执行，不默认派生子代理，不创建 worktree。

**Goal:** 用真实隔离 Redis/Sentinel 验证初始化、认证、晋升、持久角色和冷恢复，决定是否允许继续实现生产 Chart。

**Architecture:** 此计划只实现可复现的实验夹具，不实现生产启动包装器、初始化 Job 或状态 ConfigMap。六个测试容器处于独立 Compose 网络，Redis/Sentinel 分别持久化可写配置；初始配置只在空实验卷生成，后续启动不覆盖。真实实验确认后，再制定生产协议及 Chart 实施计划，不能以本夹具代替卷身份校验或初始化授权。

**Tech Stack:** Docker Compose、现有本地 `redis:7-alpine`、Redis CLI、中文实验报告。

---

## 范围、前提与文件边界

- 依据：`docs/superpowers/specs/2026-09-13-built-in-redis-sentinel-design.md` 与 `docs/testing/2026-09-13-apparmor-sentinel-design-review.md`。
- 只新增 `testdata/sentinel-feasibility/compose.yaml`、`testdata/sentinel-feasibility/redis-0.conf`、`redis-1.conf`、`redis-2.conf`、`sentinel.conf` 和实验报告。所有仓库文件用 apply_patch 创建。
- 不修改生产 Chart、当前集群、业务 Redis、业务卷；不构建或推送发布镜像。
- 密码 `review-only-not-production` 为公开实验常量，不引用任何线上 Secret。不在本实验中验证生产密码生成/注入安全。
- 使用唯一 Compose project 名；若发生名称冲突先停止，不能删除已有同名资源。实验容器/网络/卷只由该 project 管理，无宿主机端口映射。
- 当前 Docker 为 linux/aarch64，已有 Redis 镜像；AppArmor 不在 Docker SecurityOptions 中。该事实不阻断 Redis 实验，也不允许据此宣布 AppArmor 约束通过。

### Task 1: 创建持久化实验夹具

**Files:** Create: `testdata/sentinel-feasibility/{compose.yaml,redis-0.conf,redis-1.conf,redis-2.conf,sentinel.conf}`。

- [ ] 确认 Compose 可用并记录 Redis 实际版本，失败则不启动实验。

```bash
docker compose version
docker run --rm --pull=never redis:7-alpine redis-server --version
```

- [ ] 用 apply_patch 创建 `compose.yaml`，完整内容如下。Redis 和 Sentinel 各自保持 PID 1 的 exec 服务，持久配置只在首次不存在时复制。

```yaml
x-redis: &redis
  image: redis:7-alpine
  pull_policy: never
  entrypoint: ["/bin/sh", "-ec"]
  command:
    - 'test -f /data/redis.conf || cp /templates/redis.conf /data/redis.conf; exec redis-server /data/redis.conf'
x-sentinel: &sentinel
  image: redis:7-alpine
  pull_policy: never
  entrypoint: ["/bin/sh", "-ec"]
  command:
    - 'test -f /data/sentinel.conf || cp /templates/sentinel.conf /data/sentinel.conf; exec redis-server /data/sentinel.conf --sentinel'
services:
  redis-0:
    <<: *redis
    volumes:
      - redis-0:/data
      - ./redis-0.conf:/templates/redis.conf:ro
  redis-1:
    <<: *redis
    volumes:
      - redis-1:/data
      - ./redis-1.conf:/templates/redis.conf:ro
  redis-2:
    <<: *redis
    volumes:
      - redis-2:/data
      - ./redis-2.conf:/templates/redis.conf:ro
  sentinel-0:
    <<: *sentinel
    volumes:
      - sentinel-0:/data
      - ./sentinel.conf:/templates/sentinel.conf:ro
  sentinel-1:
    <<: *sentinel
    volumes:
      - sentinel-1:/data
      - ./sentinel.conf:/templates/sentinel.conf:ro
  sentinel-2:
    <<: *sentinel
    volumes:
      - sentinel-2:/data
      - ./sentinel.conf:/templates/sentinel.conf:ro
volumes:
  redis-0: {}
  redis-1: {}
  redis-2: {}
  sentinel-0: {}
  sentinel-1: {}
  sentinel-2: {}
```

- [ ] 用 apply_patch 创建 `redis-0.conf`：

```text
bind 0.0.0.0
protected-mode yes
port 6379
dir /data
appendonly yes
appendfsync everysec
requirepass "review-only-not-production"
masterauth "review-only-not-production"
min-replicas-to-write 1
min-replicas-max-lag 5
replica-announce-ip redis-0
replica-announce-port 6379
```

- [ ] 用 apply_patch 创建 `redis-1.conf`：

```text
bind 0.0.0.0
protected-mode yes
port 6379
dir /data
appendonly yes
appendfsync everysec
requirepass "review-only-not-production"
masterauth "review-only-not-production"
min-replicas-to-write 1
min-replicas-max-lag 5
replica-announce-ip redis-1
replica-announce-port 6379
replicaof redis-0 6379
```

- [ ] 用 apply_patch 创建 `redis-2.conf`：

```text
bind 0.0.0.0
protected-mode yes
port 6379
dir /data
appendonly yes
appendfsync everysec
requirepass "review-only-not-production"
masterauth "review-only-not-production"
min-replicas-to-write 1
min-replicas-max-lag 5
replica-announce-ip redis-2
replica-announce-port 6379
replicaof redis-0 6379
```

- [ ] 用 apply_patch 创建 `sentinel.conf`。Sentinel 的固定公告名按实例另行验证，此阶段先检验 Redis 稳定 DNS 被持久化及返回的行为，不声称已覆盖生产 Pod 公告地址配置。

```text
bind 0.0.0.0
protected-mode yes
port 26379
dir /data
requirepass "review-only-not-production"
sentinel sentinel-pass "review-only-not-production"
sentinel resolve-hostnames yes
sentinel announce-hostnames yes
sentinel monitor review-master redis-0 6379 2
sentinel auth-pass review-master "review-only-not-production"
sentinel down-after-milliseconds review-master 3000
sentinel failover-timeout review-master 15000
sentinel parallel-syncs review-master 1
```

- [ ] 验证 YAML，然后设置当前终端唯一 project；保留此变量直到清理完成，所有 Compose 操作必须同时带 `-p` 与 `-f`。

```bash
task_project="sandbox-sentinel-review-$(date +%Y%m%d%H%M%S)-$$"
task_compose="testdata/sentinel-feasibility/compose.yaml"
task_auth="review-only-not-production"
docker compose -p "$task_project" -f "$task_compose" config --quiet
docker ps -a --filter "label=com.docker.compose.project=$task_project"
docker volume ls --filter "label=com.docker.compose.project=$task_project"
```

预期：config 返回 0，资源列表为空。非空时终止，不执行清理命令。

### Task 2: 首次引导、认证及实际副本确认

- [ ] 先只启动三个 Redis，验证主/从复制无需 Sentinel 预先 Ready；再启动三个 Sentinel。

```bash
docker compose -p "$task_project" -f "$task_compose" up -d redis-0 redis-1 redis-2
docker compose -p "$task_project" -f "$task_compose" exec -T -e "REDISCLI_AUTH=$task_auth" redis-0 redis-cli INFO replication
docker compose -p "$task_project" -f "$task_compose" up -d sentinel-0 sentinel-1 sentinel-2
docker compose -p "$task_project" -f "$task_compose" exec -T -e "REDISCLI_AUTH=$task_auth" sentinel-0 redis-cli -p 26379 SENTINEL CKQUORUM review-master
docker compose -p "$task_project" -f "$task_compose" exec -T -e "REDISCLI_AUTH=$task_auth" sentinel-0 redis-cli -p 26379 SENTINEL MASTER review-master
```

预期：主报告两个 online slave；Sentinel 在 60 秒总预算内报告足够 quorum/majority 和两个其它 Sentinel。瞬时未收敛按 1 秒间隔重新读取，超时记 FAIL；执行等待工具单次不超过 10 秒，并持续提供进度。

- [ ] 无凭据 Redis PING 和 Sentinel PING 均返回 NOAUTH；注意 redis-cli 可能仍返回进程码 0，必须检查响应正文。

```bash
docker compose -p "$task_project" -f "$task_compose" exec -T redis-0 redis-cli PING
docker compose -p "$task_project" -f "$task_compose" exec -T sentinel-0 redis-cli -p 26379 PING
```

- [ ] 同一 CLI 连接先写后 WAIT，不能拆成两次 redis-cli 调用。

```bash
docker compose -p "$task_project" -f "$task_compose" exec -T -e "REDISCLI_AUTH=$task_auth" redis-0 redis-cli --raw <<'REDIS'
SET review:durability before-failover
WAIT 1 3000
REDIS
```

预期：SET=OK，WAIT>=1。通过复制 INFO、Sentinel 成员列表共同记录四条认证链路正常；本阶段不替代不同密码/特殊字符密码及 Go client 字段回归。

### Task 3: 晋升、旧主重返及非 0 号冷恢复

- [ ] 停止原主，60 秒预算内读取全部三个 Sentinel 的 MASTER 与 get-master-addr-by-name；只观察 Sentinel 晋升，不手动 REPLICAOF NO ONE。

```bash
docker compose -p "$task_project" -f "$task_compose" stop redis-0
docker compose -p "$task_project" -f "$task_compose" exec -T -e "REDISCLI_AUTH=$task_auth" sentinel-0 redis-cli -p 26379 SENTINEL get-master-addr-by-name review-master
docker compose -p "$task_project" -f "$task_compose" exec -T -e "REDISCLI_AUTH=$task_auth" sentinel-1 redis-cli -p 26379 SENTINEL MASTER review-master
docker compose -p "$task_project" -f "$task_compose" exec -T -e "REDISCLI_AUTH=$task_auth" sentinel-2 redis-cli -p 26379 SENTINEL MASTER review-master
```

预期：新主为 redis-1 或 redis-2；取得地址只是发现证据，必须另读其 INFO replication，确认 master/健康副本并在其同连接完成 SET/WAIT。

- [ ] 将返回的服务名精确验证为 redis-1 或 redis-2 后设 `task_master`，再验证晋升后的写入和复制。

```bash
task_master="$(docker compose -p "$task_project" -f "$task_compose" exec -T -e "REDISCLI_AUTH=$task_auth" sentinel-0 redis-cli --raw -p 26379 SENTINEL get-master-addr-by-name review-master | tr -d '\r' | head -n 1)"
case "$task_master" in redis-1|redis-2) ;; *) exit 1 ;; esac
docker compose -p "$task_project" -f "$task_compose" exec -T -e "REDISCLI_AUTH=$task_auth" "$task_master" redis-cli INFO replication
docker compose -p "$task_project" -f "$task_compose" exec -T -e "REDISCLI_AUTH=$task_auth" "$task_master" redis-cli --raw <<'REDIS'
SET review:durability after-failover
WAIT 1 3000
REDIS
docker compose -p "$task_project" -f "$task_compose" start redis-0
docker compose -p "$task_project" -f "$task_compose" exec -T -e "REDISCLI_AUTH=$task_auth" redis-0 redis-cli INFO replication
```

预期：原主最终为从且 master_host 指向新主；不能把旧主短暂启动状态或异步数据一致性写成零中断/强一致。

- [ ] 收敛后读取每个 Redis 的 `/data/redis.conf` 和 Sentinel 的 `/data/sentinel.conf`，核对角色重写、myid/config-epoch/current-epoch，并记录是否仍存旧 IP。随后停止全组、保留卷重启。

```bash
docker compose -p "$task_project" -f "$task_compose" exec -T redis-0 cat /data/redis.conf
docker compose -p "$task_project" -f "$task_compose" exec -T redis-1 cat /data/redis.conf
docker compose -p "$task_project" -f "$task_compose" exec -T redis-2 cat /data/redis.conf
docker compose -p "$task_project" -f "$task_compose" exec -T sentinel-0 cat /data/sentinel.conf
docker compose -p "$task_project" -f "$task_compose" exec -T sentinel-1 cat /data/sentinel.conf
docker compose -p "$task_project" -f "$task_compose" exec -T sentinel-2 cat /data/sentinel.conf
docker compose -p "$task_project" -f "$task_compose" stop
docker compose -p "$task_project" -f "$task_compose" up -d
```

预期：60 秒内恢复非 0 号主的一主两从、原值 after-failover、CKQUORUM 和 SET/WAIT。若 Redis 角色未被持久化或冷启动不能收敛，记录阻断项，不能通过增加默认主回退绕过。通过一次不等于已经验证 PVC 丢失或配置 epoch 分歧。

### Task 4: 有界异常与证据交付

**Files:** Create: `docs/testing/2026-09-13-sentinel-feasibility-results.md`。

- [ ] 停止除 `task_master` 外的两个 Redis，向剩余主执行 SET，预期返回 NOREPLICAS；其本地 PING 应仍可成功。此实验不删除任何卷。

```bash
case "$task_master" in
  redis-1) docker compose -p "$task_project" -f "$task_compose" stop redis-0 redis-2 ;;
  redis-2) docker compose -p "$task_project" -f "$task_compose" stop redis-0 redis-1 ;;
  *) exit 1 ;;
esac
docker compose -p "$task_project" -f "$task_compose" exec -T -e "REDISCLI_AUTH=$task_auth" "$task_master" redis-cli SET review:must-reject isolated
docker compose -p "$task_project" -f "$task_compose" exec -T -e "REDISCLI_AUTH=$task_auth" "$task_master" redis-cli PING
```
- [ ] 以 apply_patch 记录 Redis 实际版本、唯一 project、所有命令结果、收敛时间、持久角色/epoch、FAIL 与尚未测试项；不把未测试的 Pod IP 换代、特殊密码、初始化身份、防重放、状态丢失、Kubernetes 节点故障标为通过。
- [ ] 清理前核对 project 的六个容器、六个卷和独立网络标签。仅在确认全部为本次新建实验资源后，用同一 project/file 删除实验资源；不碰其它容器、缓存镜像或卷。

```bash
docker compose -p "$task_project" -f "$task_compose" ps -a
docker volume ls --filter "label=com.docker.compose.project=$task_project"
docker network ls --filter "label=com.docker.compose.project=$task_project"
docker compose -p "$task_project" -f "$task_compose" down --volumes
docker ps -a --filter "label=com.docker.compose.project=$task_project"
docker volume ls --filter "label=com.docker.compose.project=$task_project"
git diff --check
```

- [ ] 按实际结果决定生产计划：成功只解除基础切换/正常冷恢复门槛；初始化身份协议、部分引导续跑和丢失状态门槛仍必须另做确定性及真实实验。
- [ ] 完整读回夹具和报告并核对检查结果后，只提交上述实验文件，不推送、不修改 release。
