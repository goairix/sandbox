---
name: sandbox-security-audit
description: Run a comprehensive security audit on a sandbox container and generate a risk report. Use when asked to perform security checks, audit sandbox isolation, or verify security configurations.
compatibility: Requires a running Sandbox API service and curl/jq
allowed-tools: Bash(curl:*) Bash(jq:*)
metadata:
  author: goairix
  version: "1.0"
---

# Sandbox Security Audit

对 sandbox 容器进行全面安全检测，输出结构化风险报告。

## 使用方式

```bash
SANDBOX_API_URL="${SANDBOX_API_URL:-http://localhost:8080}"
SANDBOX_API_KEY="${SANDBOX_API_KEY:-}"
```

## 检测流程

通过以下 bash 脚本在 sandbox 中逐项执行检测，记录每项结果，最终汇总为报告。

### 执行单项检测的模板

```bash
curl -s -X POST "${SANDBOX_API_URL}/api/v1/execute" \
  -H "Authorization: Bearer ${SANDBOX_API_KEY}" \
  -H "Content-Type: application/json" \
  -d "$(jq -n --arg code "$CHECK_CODE" '{language: "bash", code: $code}')" \
  | jq -r '.stdout // .stderr'
```

---

## 检测项目

按顺序执行以下每个 `CHECK_CODE`，记录输出，按风险等级判定。

### 1. 云元数据接口

```bash
timeout 3 curl -s --connect-timeout 2 http://169.254.169.254/latest/meta-data/instance-id 2>/dev/null \
  && echo "STATUS:ACCESSIBLE" || echo "STATUS:BLOCKED"
```

**判定**：输出含 `STATUS:ACCESSIBLE` → 🔴 高危；`STATUS:BLOCKED` → ✅

---

### 2. user-data 密码泄露

```bash
timeout 3 curl -s --connect-timeout 2 http://169.254.169.254/latest/user-data 2>/dev/null \
  && echo "STATUS:ACCESSIBLE" || echo "STATUS:BLOCKED"
```

**判定**：输出含密码字段（password/passwd） → 🔴 高危；BLOCKED → ✅

---

### 3. 内网服务访问（RFC1918）

```bash
TARGET="172.16.20.114"
for port in 22 3306 5432 6379 9200 15672 27017; do
  result=$(timeout 2 bash -c "cat < /dev/null > /dev/tcp/${TARGET}/${port}" 2>&1 && echo "OPEN" || echo "CLOSED")
  echo "${TARGET}:${port} ${result}"
done
```

**判定**：任意端口 OPEN → 🔴 高危；全部 CLOSED → ✅

---

### 4. DNS 配置

```bash
cat /etc/resolv.conf
```

**判定**：nameserver 含 `10.96.x.x`（集群内DNS）→ 🟡 中危（泄露集群拓扑）；仅含公网DNS（8.8.8.8/1.1.1.1）→ ✅

---

### 5. 集群内部服务名解析

```bash
getent hosts kubernetes.default.svc.cluster.local 2>/dev/null \
  && echo "STATUS:RESOLVABLE" || echo "STATUS:BLOCKED"
```

**判定**：RESOLVABLE → 🟡 中危；BLOCKED → ✅

---

### 6. Seccomp 状态

```bash
grep Seccomp /proc/self/status
```

**判定**：`Seccomp: 2` → ✅ 过滤模式；`Seccomp: 0` → 🔴 高危（未启用）

---

### 7. 内存限制

```bash
cat /sys/fs/cgroup/memory.max 2>/dev/null || cat /sys/fs/cgroup/memory/memory.limit_in_bytes 2>/dev/null
```

**判定**：`max` 或超大数值（9223372036854771712）→ 🔴 高危（无限制）；具体数值 → ✅

---

### 8. CPU 限制

```bash
cat /sys/fs/cgroup/cpu.max 2>/dev/null || echo "not available"
```

**判定**：`max 100000`（无限制）→ 🟡 中危；具体 quota 数值 → ✅

---

### 9. 进程数限制

```bash
cat /sys/fs/cgroup/pids.max 2>/dev/null || cat /sys/fs/cgroup/pids/pids.max 2>/dev/null
```

**判定**：`max` 或超大数值 → 🟡 中危；≤ 200 → ✅

---

### 10. SA Token 挂载

```bash
ls /var/run/secrets/kubernetes.io/serviceaccount/ 2>/dev/null \
  && echo "STATUS:MOUNTED" || echo "STATUS:NOT_MOUNTED"
```

**判定**：MOUNTED → 🟡 中危；NOT_MOUNTED → ✅

---

### 11. 服务地址环境变量泄露

```bash
env | grep -E "_SERVICE_HOST|_SERVICE_PORT|_TCP_ADDR" | head -5
```

**判定**：有输出 → 🟡 中危（泄露集群内部地址）；无输出 → ✅

---

### 12. 容器权限（Capabilities）

```bash
grep CapEff /proc/self/status | awk '{print $2}'
```

**判定**：非 `0000000000000000` → 需进一步分析；全零 → ✅

---

### 13. /etc/passwd 可写性

```bash
test -w /etc/passwd && echo "WRITABLE" || echo "READONLY"
```

**判定**：WRITABLE → 🟡 中危；READONLY → ✅

---

### 14. Fork bomb 防护（进程创建）

```bash
# 快速测试进程创建是否受限，不触发真正的 fork bomb
pids_max=$(cat /sys/fs/cgroup/pids.max 2>/dev/null || echo "max")
current=$(cat /sys/fs/cgroup/pids.current 2>/dev/null || echo "unknown")
echo "pids.max=${pids_max} pids.current=${current}"
```

**判定**：pids.max 为 `max` → 🔴 高危；有具体上限 → ✅

---

### 15. 磁盘配额

```bash
df -h /workspace 2>/dev/null | tail -1
```

**判定**：Size 显示容量且有限制 → ✅；无限制 → 🟡

---

## 报告格式

执行完所有检测后，按以下格式输出报告：

```
# Sandbox 安全检测报告
检测时间: <timestamp>
整体风险: 🔴高危 / 🟡中危 / 🟢低危

## 检测结果汇总

| 检测项 | 结果 | 风险等级 | 详情 |
|--------|------|---------|------|
| 云元数据接口 | ✅/❌ | 低/高 | ... |
| user-data密码 | ... | ... | ... |
| 内网服务访问 | ... | ... | ... |
| DNS配置 | ... | ... | ... |
| 集群服务名解析 | ... | ... | ... |
| Seccomp | ... | ... | ... |
| 内存限制 | ... | ... | ... |
| CPU限制 | ... | ... | ... |
| 进程数限制 | ... | ... | ... |
| SA Token | ... | ... | ... |
| 服务地址泄露 | ... | ... | ... |
| Capabilities | ... | ... | ... |
| /etc/passwd权限 | ... | ... | ... |
| Fork bomb防护 | ... | ... | ... |
| 磁盘配额 | ... | ... | ... |

## 高危问题（需立即修复）
<列出所有🔴问题及建议>

## 中危问题（一周内修复）
<列出所有🟡问题及建议>

## 整体评估
<总结当前安全状态>
```

## 风险评级规则

- 🔴 **高危**：任意高危项存在 → 整体高危
- 🟡 **中危**：无高危但有中危项 → 整体中危  
- 🟢 **低危**：全部通过 → 整体低危
