#!/usr/bin/env bash
set -euo pipefail

repo_root="$(CDPATH= cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)"
chart="$repo_root/deploy/helm/sandbox"

python3 - "$chart" <<'PY'
import base64
import json
import subprocess
import sys

import yaml

chart = sys.argv[1]
prefix = "SANDBOX_STORAGE_STATE_REDIS_"
auth_names = {prefix + name for name in (
    "USERNAME", "PASSWORD", "SENTINEL_USERNAME", "SENTINEL_PASSWORD",
)}
# Deterministic dummy credentials only; helm template never connects to Redis.
data_password = 'data password "\\值'
sentinel_password = 'sentinel password "\\值'
sentinel_username = 'sentinel-user "\\值'


def render(overrides):
    command = ["helm", "template", "sandbox", chart]
    for key, value in overrides.items():
        command += ["--set-json", key + "=" + json.dumps(value)]
    return [doc for doc in yaml.safe_load_all(
        subprocess.check_output(command, text=True)) if doc]


def literal(suffix, value):
    return {"name": prefix + suffix, "value": value}


def secret_ref(suffix, key):
    return {"name": prefix + suffix, "valueFrom": {
        "secretKeyRef": {"name": "sandbox-redis", "key": key},
    }}


def check(name, overrides, expected_auth, expected_passwords):
    docs = render(overrides)
    expected_auth = {entry["name"]: entry for entry in expected_auth}
    deployment = next(doc for doc in docs if doc["kind"] == "Deployment"
                      and doc["metadata"]["name"] == "sandbox-api")
    api = deployment["spec"]["template"]["spec"]["containers"][0]
    targets = [("API", api)]
    for doc in docs:
        if doc["kind"] == "Job":
            for container in doc["spec"]["template"]["spec"]["containers"]:
                if container["name"] == "drain":
                    targets.append((doc["metadata"]["name"], container))
    assert len(targets) >= 3, (name, "API and both drain paths must be tested")
    for target_name, container in targets:
        env = container["env"]
        names = [entry["name"] for entry in env]
        assert len(names) == len(set(names)), (name, target_name, "duplicate env")
        actual = {entry["name"]: entry for entry in env if entry["name"] in auth_names}
        assert actual == expected_auth, (name, target_name,
                                        "authentication env mismatch", actual, expected_auth)
    secrets = [doc for doc in docs if doc["kind"] == "Secret"
               and doc["metadata"]["name"] == "sandbox-redis"]
    if expected_passwords:
        assert len(secrets) == 1, (name, "one Redis auth Secret required")
        decoded = {key: base64.b64decode(value, validate=True).decode()
                   for key, value in secrets[0]["data"].items()}
        assert decoded == expected_passwords, (name, "independent Secret keys", decoded)
    else:
        assert not secrets, (name, "unconfigured Redis auth Secret")


external = {"redis.enabled": False, "redis.external.mode": "sentinel",
            "redis.external.masterName": "sandbox-master",
            "redis.external.addr": "sentinel.example:26379"}
check("distinct external credentials", {
    **external, "redis.external.username": "data-user",
    "redis.external.password": data_password,
    "redis.external.sentinelUsername": sentinel_username,
    "redis.external.sentinelPassword": sentinel_password,
}, [literal("USERNAME", "data-user"), secret_ref("PASSWORD", "password"),
    literal("SENTINEL_USERNAME", sentinel_username),
    secret_ref("SENTINEL_PASSWORD", "sentinel-password")],
    {"password": data_password, "sentinel-password": sentinel_password})
check("Sentinel password only", {
    **external, "redis.external.sentinelPassword": sentinel_password,
}, [secret_ref("SENTINEL_PASSWORD", "sentinel-password")],
    {"sentinel-password": sentinel_password})
check("data password only", {
    **external, "redis.external.password": data_password,
}, [secret_ref("PASSWORD", "password")], {"password": data_password})
check("Sentinel username only", {
    **external, "redis.external.sentinelUsername": sentinel_username,
}, [literal("SENTINEL_USERNAME", sentinel_username)], {})
check("external without auth", external, [], {})
check("default built-in", {}, [], {})
check("built-in ignores external Sentinel auth", {
    "redis.external.sentinelUsername": sentinel_username,
    "redis.external.sentinelPassword": sentinel_password,
}, [], {})
check("built-in data password unchanged", {"redis.password": data_password},
      [secret_ref("PASSWORD", "password")], {"password": data_password})
for mode in ("standalone", "cluster"):
    result = subprocess.run(["helm", "template", "sandbox", chart,
                             "--set", "redis.enabled=false",
                             "--set", "redis.external.mode=" + mode,
                             "--set", "redis.external.masterName=leftover"],
                            capture_output=True, text=True)
    assert result.returncode != 0, (mode, "conflicting masterName accepted")
    assert "masterName is only valid in sentinel mode" in result.stderr
PY

# Installed Deployments retain their actual complete env, including optional auth.
rg -Fq '{{- if hasKey $installedContainer "env" }}' "$chart/templates/pre-backend-change-drain.yaml"
rg -Fq '{{ toYaml (get $installedContainer "env") | nindent 12 }}' "$chart/templates/pre-backend-change-drain.yaml"
printf 'helm external Sentinel auth tests: PASS\n'
