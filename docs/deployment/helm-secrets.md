# Helm 凭据配置、Secret 创建与字节校验

本页配合 [生产部署手册](helm-production.md) 使用。默认内置 etcd + FUSE 需要对象存储凭据和 API key；私有仓库按节点拉取方式准备认证。外部 etcd mTLS、Redis 认证只在对应模式使用。

## 0. 可以直接写环境 values，不必手工创建 Secret

当前 Chart 同时支持两种方式，可按用途选择，不要求所有密钥都手工建 Secret：

| 凭据 | 直接写 values 的字段 | Chart 如何使用 |
| --- | --- | --- |
| 对象存储 AK/SK | `config.storage.filesystem.accessKey/secretKey` | 直接注入 API 环境，不创建对象存储凭据 Secret；`credentialsSecretName` 必须为空 |
| API key | `config.security.apiKey` | Chart 自动创建 `<release>-api` Secret 并引用；`apiKeySecretName` 必须为空 |
| Redis/Sentinel 密码 | 内置 `redis.password`、`redis.sentinel.password`；外部 `redis.external.password/sentinelPassword` | Chart 自动创建 `<release>-redis` Secret；内置 Sentinel 的 `existingSecret` 保持为空 |
| 默认内置 etcd | 无需填写认证 | 不需要认证 Secret |
| 仓库认证、外部 etcd mTLS | 现有接口引用 Secret 名称 | 不提供明文用户名/密码或证书直接写 values 后自动创建对应 Secret 的接口；节点已具备仓库凭据时可不配置拉取 Secret |

**不需要手工创建 Secret，不等于集群里完全没有 Secret。** API key 和 Redis 认证由 Helm 自动管理即可。Sentinel 身份仍由安装 Job 生成和复用，不手工重建。

直接填写时，在 Chart 外的环境 `values-prod.yaml` 中合并以下字段（替换示例值）。生产 overlay 已预填 Secret 引用，所以这两个名称必须显式清空，否则 AK/SK 会冲突，API key 仍会使用引用的旧 Secret。

```yaml
config:
  storage:
    filesystem:
      credentialsSecretName: ''
      accessKey: 'REPLACE_WITH_ACCESS_KEY'
      secretKey: 'REPLACE_WITH_SECRET_KEY'
  security:
    apiKeySecretName: ''
    apiKey: 'REPLACE_WITH_YOUR_API_KEY'
```

这是对环境文件中已有字段的替换，不要在同一个 YAML 内重复创建顶层 `config`。沿用生产手册原来的 `-f values-production.yaml -f "$VALUES"` 顺序即可；对象存储/API key 的手工 Secret 创建步骤可以跳过，仓库或 TLS Secret 仅按实际需要处理。

密码按单行字符串填写，不用 `|` / `>` 多行块。单引号内若包含单引号，用两个单引号表示，例如 `'ab''cd'`；YAML 文件最后正常有换行，不会给这些单行值附加换行。直接填写的密钥会进入环境文件、Helm release 历史或渲染结果，环境文件按凭据文件保管（例如 `0600`，不提交 Git）。对象存储 AK/SK 轮换仍须递增 `credentialGeneration` 并按正常升级流程处理。

只有选择“引用已有 Secret”时，才按下面的文件准备、创建和回读流程执行；两种方式不要在同一用途上同时配置。

## 1. 先区分 Secret 的用途与键名

所有引用的 Secret 必须位于 API 的 release namespace。本生产示例中 runtime 也使用该 namespace；如果另设 runtime namespace，需要在那个 namespace 单独准备拉取 Secret。

| 用途 | 示例 Secret 名 | 必须存在的键 | 对应 values |
| --- | --- | --- | --- |
| 对象存储 AK/SK，选择引用 Secret 时 | `sandbox-storage-credentials` | `access-key`、`secret-key` | `config.storage.filesystem.credentialsSecretName` |
| API key，选择引用 Secret 时 | `sandbox-api-key` | `api-key` | `config.security.apiKeySecretName` |
| API key，由 Chart 自动创建 | `<release>-api` | `api-key` | `config.security.apiKey`，且 `apiKeySecretName` 为空 |
| 私有镜像仓库，按需 | `sandbox-registry` | `.dockerconfigjson`，类型为 `kubernetes.io/dockerconfigjson` | `image.pullSecretName`；runtime/loader 的权限边界见下文 |
| 外部 etcd mTLS，按需 | `sandbox-etcd-client-tls` | `ca.crt`、`client.crt`、`client.key` | `config.storage.state.etcd.existingSecret`，且 `etcd.enabled: false` |
| 内置 Redis Sentinel 认证，兼容模式按需 | `sandbox-redis-auth` | 默认 `password`、`sentinel-password`，两套密码不同 | `redis.sentinel.existingSecret`、`dataPasswordKey`、`sentinelPasswordKey` |
| 内置 Sentinel 身份，由安装 Job 自动生成 | `<release>-redis-sentinel-identity` | 程序定义的成员身份材料 | `redis.sentinel.identitySecretName: ""`；不手工生成、修改或重新创建 |
| 外部 Redis/Sentinel 或内置 Redis inline 认证，由 Chart 生成 | `<release>-redis` | `password` 和/或 `sentinel-password`，按配置渲染 | 外部使用 `redis.external.password/sentinelPassword`；内置 inline 使用 `redis.password/redis.sentinel.password` |

默认内置 etcd 使用隔离的内部 HTTP，**无需创建 etcd 密码或 TLS Secret**。Sentinel 的“身份 Secret”和“认证 Secret”用途不同；已有 PVC 的安装必须保留原身份，缺失时恢复原对象，不能重新生成替代。

## 2. 为什么看起来没有换行，实际却多了一个

`kubectl create secret --from-file` 会按文件内容保存字节，不会为密码主动补换行。常见来源是：

- `echo "$VALUE" > file` 会在值后追加 LF（`0a`）；`openssl rand -base64 ... > file` 的输出也常以换行结束。
- 编辑器保存时自动补文件末尾换行；从 Windows 复制的文件可能有 CRLF（`0d 0a`）。
- YAML `stringData` 中用 `|` 会保留末尾换行；单行密码应使用正确引用的单行标量，或 `|-` 去掉块标量末尾换行。**Secret YAML 文件本身以换行结尾，不等于 data 里的密码带换行。**
- Secret 管理工具或复制粘贴流程可能在创建前改变内容；需要比较真实字节，不能凭界面显示判断。

这些行为见 [Kubernetes Secret 创建说明](https://kubernetes.io/docs/tasks/configmap-secret/managing-secret-using-kubectl/) 与 [YAML 块标量规范](https://yaml.org/spec/1.2.2/#block-chomping-indicator)。已有问题的具体来源需用下文的回读比较定位。

单行 AK/SK、API key、Redis 密码应拒绝 CR/LF/NUL；**不要对 PEM 证书、私钥或 JSON 文件执行统一去换行**。避免用 `echo` 写密码；已有 shell 变量时使用 `printf '%s' "$VALUE"`。不要用 `VALUE=$(kubectl ... | base64 -d)` 检测结尾换行：命令替换会移除结尾 LF，掩盖问题。不要用 `.strip()` 或 `tr -d '\r\n'` 自动“修正”所有凭据，以免改变合法内容。

## 3. 准备单行凭据文件，不写入末尾换行

需要 `python3` 和 `kubectl`。先按生产手册确认 context/namespace；以下变量在同一终端后续继续使用。`SECRET_DIR` 是受控文件目录，不能放进 Git。

```bash
CTX=your-production-context
NS=sandbox-prod
RELEASE=sandbox
SECRET_DIR=/secure/sandbox
```

以下程序在交互式终端执行（通过 `/dev/tty` 隐藏读取）；无人值守发布应从凭据管理系统取得原始文件，再执行本地检查。程序交互读取 AK、SK 和 API key，各输入两次，使用 UTF-8 原始字节写入，不调用 `print` 写密码、不附加换行。API key 使用已由你生成并交给调用方的强随机 token；这里不改变已有 API key。已有同名文件时拒绝覆盖，避免误替换环境凭据。

```bash
python3 - "$SECRET_DIR" <<'PY'
import getpass
import os
import pathlib
import sys

root = pathlib.Path(sys.argv[1])
root.mkdir(mode=0o700, parents=True, exist_ok=True)
os.chmod(root, 0o700)
labels = {
    "access-key": "对象存储 access key",
    "secret-key": "对象存储 secret key",
    "api-key": "API key",
}
if any((root / name).exists() for name in labels):
    raise SystemExit("目标文件已存在；先核对已有文件或使用新的受控目录。")
values = {}
for name, label in labels.items():
    value = getpass.getpass(label + ": ")
    confirm = getpass.getpass("再次输入 " + label + ": ")
    if value != confirm or not value or any(c in value for c in "\r\n\x00"):
        raise SystemExit(label + " 为空、含 CR/LF/NUL 或两次输入不一致，未写入。")
    values[name] = value.encode("utf-8")
for name, raw in values.items():
    fd = os.open(root / name, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
    with os.fdopen(fd, "wb") as stream:
        stream.write(raw)
    print(name + ": 已写入，未附加换行")
PY
```

从凭据管理系统下载文件时可跳过交互写入，但仍必须按二进制检查。不要用 `cat` 或终端十六进制转储输出真实凭据。以下检查只输出文件名、字节数和换行状态；存在异常即退出：

```bash
python3 - "$SECRET_DIR" <<'PY'
import pathlib
import sys

root = pathlib.Path(sys.argv[1])
failed = False
for name in ("access-key", "secret-key", "api-key"):
    raw = (root / name).read_bytes()
    has_cr_lf = b"\r" in raw or b"\n" in raw
    invalid = not raw or has_cr_lf or b"\x00" in raw
    print(f"{name}: bytes={len(raw)}, CR/LF={has_cr_lf}, valid={not invalid}")
    failed |= invalid
if failed:
    raise SystemExit("存在不合法的单行凭据；从可信来源重新准备，不自动裁剪。")
PY
```

## 4. 创建默认部署需要的 Secret

文件检查通过后执行。`--from-file` 后面是**文件路径**，文件内容必须是原始值，不能预先做 base64 编码；kubectl 会填充 Secret 的 `data`。

```bash
kubectl --context "$CTX" -n "$NS" create secret generic sandbox-storage-credentials \
  --from-file=access-key="$SECRET_DIR/access-key" \
  --from-file=secret-key="$SECRET_DIR/secret-key"

kubectl --context "$CTX" -n "$NS" create secret generic sandbox-api-key \
  --from-file=api-key="$SECRET_DIR/api-key"
```

已有同名 Secret 时先执行第 7 节回读比较，确认内容后复用；不要为绕过 AlreadyExists 直接删除。引用 Secret 后，values 中的 `config.storage.filesystem.accessKey/secretKey` 和 `config.security.apiKey` 保持空值，避免同时配置 inline 凭据。

## 5. 可选的仓库认证与外部 etcd TLS

### 私有镜像仓库

已有 `registry-config.json` 时，确认包含目标仓库的实际 `auths`，不是仅含 Docker Desktop 的 `credsStore/credHelpers`。Kubernetes 不能替你调用本机的凭据助手。JSON 文件的格式换行可以保留，认证字段里的用户名/密码不可含意外 CR/LF。

没有现成配置时，以下程序交互生成最小 JSON，密码不进入命令参数或 shell 历史；不验证仓库账号是否有效，发布前仍需验证实际拉取权限。

```bash
python3 - "$SECRET_DIR" <<'PY'
import base64
import getpass
import json
import os
import pathlib
import sys

path = pathlib.Path(sys.argv[1]) / "registry-config.json"
username = getpass.getpass("registry.i.huaxisy.com 用户名（不回显）: ")
password = getpass.getpass("仓库密码/token: ")
confirm = getpass.getpass("再次输入仓库密码/token: ")
if (not username or not password or ":" in username or password != confirm
        or any(c in username + password for c in "\r\n\x00")):
    raise SystemExit("用户名/密码不合法或两次输入不一致。")
auth = base64.b64encode((username + ":" + password).encode("utf-8")).decode("ascii")
config = {"auths": {"registry.i.huaxisy.com": {"auth": auth}}}
fd = os.open(path, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
with os.fdopen(fd, "wb") as stream:
    stream.write(json.dumps(config).encode("utf-8"))
print("registry-config.json: 已写入")
PY

kubectl --context "$CTX" -n "$NS" create secret generic sandbox-registry \
  --type=kubernetes.io/dockerconfigjson \
  --from-file=.dockerconfigjson="$SECRET_DIR/registry-config.json"
```

设置 `image.pullSecretName: sandbox-registry` 后，API、使用其 ServiceAccount 的 Hook 和内置 etcd Pod 会使用该 Secret。受管 runtime 使用 `default` ServiceAccount：先检查现有 `imagePullSecrets`，已有列表时合并保留；**仅在确认列表为空时**执行下例：

```bash
kubectl --context "$CTX" -n "$NS" get serviceaccount default \
  -o jsonpath='{.imagePullSecrets}{"\n"}'
kubectl --context "$CTX" -n "$NS" patch serviceaccount default \
  --type=merge -p '{"imagePullSecrets":[{"name":"sandbox-registry"}]}'
```

loader 的专用 ServiceAccount 不自动继承该设置，需平台提供节点仓库凭据或已验证的认证注入机制。另设 runtime namespace 时，在那边创建对应拉取 Secret 并配置其 ServiceAccount。

### 外部 etcd mTLS

仅 `etcd.enabled: false` 的外部集群使用。将平台签发的原始 PEM 文件放入受控目录，**保留证书和私钥的正常多行格式**；不自行编造证书，不套用单行密码去换行规则。

```bash
kubectl --context "$CTX" -n "$NS" create secret generic sandbox-etcd-client-tls \
  --from-file=ca.crt="$SECRET_DIR/ca.crt" \
  --from-file=client.crt="$SECRET_DIR/client.crt" \
  --from-file=client.key="$SECRET_DIR/client.key"
```

设置 `config.storage.state.etcd.existingSecret: sandbox-etcd-client-tls`，并配置实际外部 endpoints。证书需与服务端信任链、有效期和客户端身份要求匹配。第 7 节可逐字节比较 PEM，但不能代替 TLS 连接验收。

## 6. Redis 兼容模式的 Secret

默认 etcd 部署跳过本节。

### 内置 Sentinel：单独创建认证 Secret，身份仍由安装 Job 处理

仅适用于全新内置 Sentinel 安装。两密码必须不同，各为 32～256 位字母、数字、`_`、`-`，下面生成两个独立随机 token，文件不附加换行；已有安装必须复用原认证，不在这里重新随机生成。

```bash
python3 - "$SECRET_DIR" <<'PY'
import os
import pathlib
import secrets
import sys

root = pathlib.Path(sys.argv[1])
paths = [root / "redis-password", root / "sentinel-password"]
if any(path.exists() for path in paths):
    raise SystemExit("Redis 认证文件已存在，拒绝覆盖。")
values = [secrets.token_urlsafe(48), secrets.token_urlsafe(48)]
while values[0] == values[1]:
    values[1] = secrets.token_urlsafe(48)
for path, value in zip(paths, values):
    fd = os.open(path, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
    with os.fdopen(fd, "wb") as stream:
        stream.write(value.encode("ascii"))
print("两个独立 Redis token 已写入，未附加换行")
PY

kubectl --context "$CTX" -n "$NS" create secret generic sandbox-redis-auth \
  --from-file=password="$SECRET_DIR/redis-password" \
  --from-file=sentinel-password="$SECRET_DIR/sentinel-password"
```

在 Redis 环境 values 中合并以下配置，不与 etcd 生产 overlay 叠加：

```yaml
etcd:
  enabled: false
config:
  storage:
    state:
      backend: redis
redis:
  enabled: true
  mode: sentinel
  password: ""
  sentinel:
    existingSecret: sandbox-redis-auth
    dataPasswordKey: password
    sentinelPasswordKey: sentinel-password
    password: ""
    identitySecretName: "" # 安装 Job 自动创建/复用身份，不能手工换钥匙。
```

保留 Redis persistence、StorageClass、API/loader 等原有环境配置；此片段不替代完整生产 values。详细部署边界见 [内置 Sentinel 手册](built-in-redis-sentinel.md)。

### 外部 Redis/Sentinel：由 Chart 创建认证 Secret

当前 Chart 没有外部 Redis `existingSecret` 接口；`redis.sentinel.existingSecret` 只用于**内置** Sentinel。外部模式从 `redis.external.password/sentinelPassword` 读取密码，并生成 `<release>-redis`。不要提前创建同名对象导致 Helm 所有权冲突。

需要带密码的外部 Sentinel 时，用下面的交互程序生成受保护的 JSON values 覆盖文件；JSON 可直接作为 `helm -f` 的输入，不用手写 YAML `|`，也不把密码放进 `--set` 或命令行。

```bash
python3 - "$SECRET_DIR" <<'PY'
import getpass
import json
import os
import pathlib
import sys

values = {}
for key, label in (("password", "外部 Redis 数据密码"),
                   ("sentinelPassword", "外部 Sentinel 密码")):
    value = getpass.getpass(label + ": ")
    confirm = getpass.getpass("再次输入 " + label + ": ")
    if value != confirm or not value or any(c in value for c in "\r\n\x00"):
        raise SystemExit("密码为空、含 CR/LF/NUL 或两次输入不一致。")
    values[key] = value
path = pathlib.Path(sys.argv[1]) / "values-redis-auth.json"
fd = os.open(path, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
with os.fdopen(fd, "wb") as stream:
    stream.write(json.dumps({"redis": {"external": values}}).encode("utf-8"))
print("values-redis-auth.json: 已写入")
PY
```

按实际外部服务器配置用户名、端点、masterName；在使用 `values-production-redis.yaml` 的渲染/安装/升级命令中，将 `-f "$SECRET_DIR/values-redis-auth.json"` 放在环境 values 后。此方式会把认证内容保存到 Helm release 历史，限制相关读取权限。外部 standalone 仅配置数据密码、清空 Sentinel 相关参数，且不满足生产 HA 示例的要求。内置 standalone 则通过 `redis.password` 由 Chart 生成密码 Secret；不属于当前生产主线。

## 7. 创建后回读：只比较字节，不显示密码

下面同时检查默认的两个 Secret：从集群取回 JSON，base64 解码为 `bytes`，与本地原文件逐字节比较。**不经 shell 命令替换，不打印原值或密码摘要。** 输出只包含字节数、CR/LF 状态及是否一致；任何不一致都返回非零。

```bash
python3 - "$CTX" "$NS" "$SECRET_DIR" <<'PY'
import base64
import hmac
import json
import pathlib
import subprocess
import sys

context, namespace, directory = sys.argv[1:]
root = pathlib.Path(directory)
checks = {
    "sandbox-storage-credentials": {"access-key": "access-key", "secret-key": "secret-key"},
    "sandbox-api-key": {"api-key": "api-key"},
    # 按实际启用情况添加，PEM/JSON 只比较原始字节，允许换行：
    # "sandbox-registry": {".dockerconfigjson": "registry-config.json"},
    # "sandbox-etcd-client-tls": {"ca.crt": "ca.crt", "client.crt": "client.crt", "client.key": "client.key"},
    # "sandbox-redis-auth": {"password": "redis-password", "sentinel-password": "sentinel-password"},
}
text_keys = {"access-key", "secret-key", "api-key", "password", "sentinel-password"}
failed = False
for name, keys in checks.items():
    result = subprocess.run(
        ["kubectl", "--context", context, "-n", namespace, "get", "secret", name, "-o", "json"],
        check=True, capture_output=True, text=True,
    )
    data = json.loads(result.stdout).get("data", {})
    for key, filename in keys.items():
        if key not in data:
            print(f"{name}/{key}: missing")
            failed = True
            continue
        actual = base64.b64decode(data[key], validate=True)
        expected = (root / filename).read_bytes()
        same = hmac.compare_digest(actual, expected)
        has_cr_lf = b"\r" in actual or b"\n" in actual
        valid = bool(actual) and (key not in text_keys or (not has_cr_lf and b"\x00" not in actual))
        print(f"{name}/{key}: local_bytes={len(expected)}, remote_bytes={len(actual)}, "
              f"CR/LF={has_cr_lf}, identical={same}, valid={valid}")
        failed |= not same or not valid
if failed:
    raise SystemExit("Secret 内容不一致或单行凭据格式错误，停止部署并定位来源。")
PY
```

外部 Redis 的 `<release>-redis` 由 Helm 创建后再检查；可在上述映射加入真实名称，并将两个键指向可信来源的原始密码文件。仅检查 JSON 中 base64 编码字符串末尾是否有 `=`，无法判断解码后的值是否多了 LF。

若原文件无 CR/LF、但回读不一致，检查是否创建到了另一个 context/namespace、文件路径是否正确、是否有同名旧 Secret，以及发布工具/Secret operator 是否在覆盖该对象。不要打印全量 Secret YAML 或容器环境来排查。

## 8. 已有 Secret 不一致时怎么修复

先从凭据管理系统或提供方重新取得正确字节，再创建新版本 Secret，回读验证后更新对应 values。推荐例如 `sandbox-storage-credentials-v2`、`sandbox-api-key-v2`；不要直接删除正在使用的旧 Secret。

- 对象存储：更新 `credentialsSecretName`，同时递增 `credentialGeneration`，按生产手册安排 drain/upgrade。保留旧 Secret 供旧 API 的 pre-upgrade Hook 使用；旧凭据须能完成必要的 flush/清理，不能先让它失效。
- API key：更新 `apiKeySecretName`，安排调用方与 API 的密钥切换；确认滚动完成和带新 key 的实际请求通过。
- 仓库认证：更新 `image.pullSecretName`，同步合并 runtime ServiceAccount 的拉取配置，并按平台方案处理 loader。
- 外部 etcd TLS：更新证书 Secret 引用，核对服务端信任链和证书轮换窗口，验证 API/drain 到原集群的连接；不改状态集群身份。
- Redis/Sentinel：认证必须与服务器当前密码匹配，已有集群按认证轮换/恢复流程处理，不能只修改 Secret 让客户端和服务端各用一套密码；身份 Secret 不在本流程内重建。

Secret 通过环境变量注入时，已运行的 Pod 不会因 Secret 内容变更自动更新其环境。使用新 Secret 名称更新 values 后执行正常 Helm 升级，并确认 API rollout；如果坚持原名更新，需明确触发对应消费者重建和业务验收。仅执行 `kubectl create/apply secret` 不能证明应用已使用新值。原本写错的凭据无法完成清理时，按受控故障恢复处理，不用禁用 Hook 来跳过。
