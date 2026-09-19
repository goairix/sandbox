"""Render the immutable Chart profile and bind offline features to signed source."""
import ast
import hashlib
import json
import os
from pathlib import Path
import shutil
import subprocess
import tarfile
import tempfile

from source_policy import sha256

MARKER = "__SANDBOX_PROFILE_NAME__"
# This immutable upstream ABI file is retained as a userspace fixture. It is
# compatible with the profile's extended network rules, but never a claim
# about the target node kernel.
FEATURE_MEMBER = "apparmor-v4.1.7/profiles/apparmor.d/abi/4.0"
DEFAULT_MEMBER = "apparmor-v4.1.7/parser/default_features.c"
CHART_INPUTS = {"profile-source/workspace-mounter.profile", "profile-source/_helpers.tpl", "profile-source/apparmor-loader.yaml"}
RENDER_FILES = {"profile/workspace.profile", "profile/render.json"}
PROFILE_FILES = RENDER_FILES | {"profile/kernel-features.fixture", "profile/features.json", "profile/workspace.bin",
                                "profile/compile.json", "profile/compile.stdout", "profile/compile.stderr",
                                "profile/policy-features.default", "profile/policy-features.json"}
BODY_EXPRESSION = '.Files.Get "files/apparmor/workspace-mounter.profile" | replace "\\r\\n" "\\n" | trim | replace "__SANDBOX_PROFILE_NAME__" $profile'


def normalize(data):
    text = data.decode("utf-8").replace("\r\n", "\n").strip()
    if "\r" in text or "\x00" in text or text.count(MARKER) != 1:
        raise ValueError("workspace profile has invalid normalization or placeholder count")
    return (text + "\n").encode()


def source_metadata(root):
    return {name: sha256(root / name) for name in sorted(CHART_INPUTS)}


def render_profile(context):
    context = Path(context)
    original = (context / "profile-source/workspace-mounter.profile").read_bytes()
    canonical = normalize(original)
    # Fail if the production loader rendering rule changes, rather than
    # silently keeping an outdated copy of its expression in this probe.
    loader = (context / "profile-source/apparmor-loader.yaml").read_text()
    if loader.count("{{ " + BODY_EXPRESSION + " | indent 4 }}") != 1:
        raise ValueError("Chart profile rendering expression changed; review the rendering contract")
    with tempfile.TemporaryDirectory(prefix="helm-profile-", dir=str(context.parent)) as temporary:
        chart = Path(temporary)
        (chart / "templates").mkdir()
        (chart / "files/apparmor").mkdir(parents=True)
        (chart / "Chart.yaml").write_text("apiVersion: v2\nname: apparmor-profile-audit\nversion: 0.1.0\n")
        shutil.copyfile(context / "profile-source/_helpers.tpl", chart / "templates/_helpers.tpl")
        (chart / "files/apparmor/workspace-mounter.profile").write_bytes(original)
        probe = '{{- $profile := include "sandbox.effectiveLSMProfile" . -}}\n'
        probe += '{{- $digest := include "sandbox.apparmor.digest" . -}}\n'
        probe += '{{- $body := ' + BODY_EXPRESSION + ' -}}\n'
        probe += '{{ dict "profile_name" $profile "template_sha256" $digest "profile" (printf "%s\\n" $body) | toJson }}\n'
        (chart / "templates/profile.yaml").write_text(probe)
        empty_config = chart / "empty-kubeconfig"
        empty_config.write_text("apiVersion: v1\nkind: Config\nclusters: []\ncontexts: []\nusers: []\n")
        empty_config.chmod(0o600)
        # Offline Helm rendering never consults user kubeconfig, Helm repositories,
        # host plugins, or Chart values. Only this fixed enabled flag is used.
        env = {"PATH": os.environ.get("PATH", ""), "KUBECONFIG": str(empty_config), "LC_ALL": "C",
               "HELM_CONFIG_HOME": str(chart / ".helm/config"), "HELM_CACHE_HOME": str(chart / ".helm/cache"),
               "HELM_DATA_HOME": str(chart / ".helm/data"), "HELM_PLUGINS": str(chart / ".helm/plugins")}
        try:
            result = subprocess.run(["helm", "template", "sandbox", str(chart), "--set", "apparmorLoader.enabled=true"],
                                    capture_output=True, timeout=30, env=env)
            version = subprocess.run(["helm", "version", "--short"], capture_output=True, timeout=15, env=env)
        except (OSError, subprocess.TimeoutExpired):
            raise ValueError("Helm unavailable or offline profile rendering timed out") from None
        if result.returncode or result.stderr or version.returncode or version.stderr:
            raise ValueError("Helm profile rendering/version check failed or emitted a warning")
        body = "\n".join(line for line in result.stdout.decode().splitlines() if line and line != "---" and not line.startswith("#"))
        rendered = json.loads(body)
    digest = hashlib.sha256(canonical).hexdigest()
    name = "sandbox-fuse-" + digest
    expected = canonical.decode().replace(MARKER, name)
    if rendered != {"profile_name": name, "template_sha256": digest, "profile": expected}:
        raise ValueError("Helm profile differs from Chart normalization/hash/name contract")
    target = context / "profile"
    target.mkdir()
    (target / "workspace.profile").write_bytes(expected.encode())
    metadata = {"profile_name": name, "template_sha256": digest, "profile_sha256": sha256(target / "workspace.profile"),
                "source_hashes": source_metadata(context), "helm_version": version.stdout.decode().strip(),
                "normalization": "CRLF to LF, trim, append LF; hash before name substitution"}
    (target / "render.json").write_text(json.dumps(metadata, indent=2, sort_keys=True) + "\n")
    return metadata


def validate_render(root, expected_template=None):
    root = Path(root)
    inputs = root / "inputs" if (root / "inputs").is_dir() else root
    original = (inputs / "profile-source/workspace-mounter.profile").read_bytes()
    canonical = normalize(original)
    if expected_template is not None and canonical != normalize(expected_template):
        raise ValueError("artifact profile differs from the current complete Chart policy")
    metadata = json.loads((root / "profile/render.json").read_text())
    digest = hashlib.sha256(canonical).hexdigest()
    name = "sandbox-fuse-" + digest
    rendered = (root / "profile/workspace.profile").read_bytes()
    if (rendered != canonical.replace(MARKER.encode(), name.encode()) or MARKER.encode() in rendered
            or metadata.get("profile_name") != name or metadata.get("template_sha256") != digest
            or metadata.get("profile_sha256") != hashlib.sha256(rendered).hexdigest()
            or metadata.get("source_hashes") != source_metadata(inputs)):
        raise ValueError("profile content/hash/name or Chart input provenance does not match")
    return metadata


def feature_bytes(archive):
    with tarfile.open(archive, "r:gz") as source:
        member = source.getmember(FEATURE_MEMBER)
        if not member.isfile() or not 0 < member.size < 65536:
            raise ValueError("upstream features fixture is not a bounded regular source file")
        return source.extractfile(member).read()


def fixture_metadata(archive, data):
    return {"kind": "upstream-policy-abi-fixture", "source_member": FEATURE_MEMBER,
            "source_archive_sha256": sha256(archive), "sha256": hashlib.sha256(data).hexdigest(),
            "target_kernel_compatibility": False, "scope": "userspace compilation only; not a node kernel snapshot"}


def extract_feature_fixture(archive, root):
    data = feature_bytes(archive)
    target = Path(root) / "profile"
    target.mkdir(exist_ok=True)
    (target / "kernel-features.fixture").write_bytes(data)
    (target / "features.json").write_text(json.dumps(fixture_metadata(archive, data), indent=2, sort_keys=True) + "\n")


def validate_feature_fixture(archive, root):
    data = feature_bytes(archive)
    target = Path(root) / "profile"
    metadata = json.loads((target / "features.json").read_text())
    if (target / "kernel-features.fixture").read_bytes() != data or metadata != fixture_metadata(archive, data):
        raise ValueError("offline features fixture differs from its signed upstream source member")
    return metadata


def default_abi_bytes(archive):
    with tarfile.open(archive, "r:gz") as source:
        member = source.getmember(DEFAULT_MEMBER)
        if not member.isfile() or not 0 < member.size < 1024 * 1024:
            raise ValueError("default policy ABI source member is not a bounded regular file")
        text = source.extractfile(member).read().decode("utf-8")
    marker = "const char *default_features_abi"
    start = text.find(marker)
    if start < 0:
        raise ValueError("signed source has no default_features_abi definition")
    end = text.find(";", start)
    if end < 0:
        raise ValueError("default_features_abi definition is unterminated")
    fragment, literals, index = text[start:end], [], 0
    while index < len(fragment):
        if fragment[index] != '"':
            index += 1
            continue
        escaped, quote, cursor = False, [], index + 1
        while cursor < len(fragment):
            char = fragment[cursor]
            if not escaped and char == '"':
                break
            quote.append(char)
            if escaped:
                escaped = False
            elif char == "\\":
                escaped = True
            cursor += 1
        if cursor >= len(fragment):
            raise ValueError("default_features_abi has an unterminated C string")
        source_literal = '"' + ''.join(quote).replace("\\\n", "") + '"'
        try:
            literals.append(ast.literal_eval(source_literal))
        except (SyntaxError, ValueError):
            raise ValueError("default_features_abi contains an unsupported C escape") from None
        index = cursor + 1
    if len(literals) != 1 or not literals[0] or len(literals[0]) > 65536:
        raise ValueError("default_features_abi source definition is ambiguous or oversized")
    return literals[0].encode("utf-8")


def default_abi_metadata(archive, data):
    return {"kind": "source-default-policy-abi", "source_member": DEFAULT_MEMBER,
            "source_archive_sha256": sha256(archive), "sha256": hashlib.sha256(data).hexdigest(),
            "target_kernel_compatibility": False,
            "scope": "exact parser default policy ABI; explicitly supplied, never overridden"}


def extract_default_abi(archive, root):
    data = default_abi_bytes(archive)
    target = Path(root) / "profile"
    target.mkdir(exist_ok=True)
    (target / "policy-features.default").write_bytes(data)
    (target / "policy-features.json").write_text(json.dumps(default_abi_metadata(archive, data), indent=2, sort_keys=True) + "\n")


def validate_default_abi(archive, root):
    data = default_abi_bytes(archive)
    target = Path(root) / "profile"
    metadata = json.loads((target / "policy-features.json").read_text())
    if (target / "policy-features.default").read_bytes() != data or metadata != default_abi_metadata(archive, data):
        raise ValueError("policy ABI fixture differs from its signed upstream source member")
    return metadata
