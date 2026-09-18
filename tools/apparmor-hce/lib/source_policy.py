"""Source trust policy shared by the host CLI and isolated HCE builder."""
import hashlib
import os
from pathlib import Path, PurePosixPath
import re
import subprocess
import tarfile
import tempfile

ARCHIVE_URL = "https://gitlab.com/apparmor/apparmor/-/archive/v4.1.7/apparmor-v4.1.7.tar.gz"
SIGNATURE_URL = "https://gitlab.com/api/v4/projects/4484878/packages/generic/signatures/4.1.7/apparmor-v4.1.7.tar.gz.asc"
LOCK_KEYS = {"version", "release_archive_url", "signature_url", "archive_sha256",
             "trusted_public_key_fingerprint", "trusted_signing_key_fingerprint",
             "source_revision", "source_revision_verification"}


def read_lock(path):
    try:
        data = Path(path).read_text(encoding="ascii")
    except (OSError, UnicodeError):
        raise ValueError("source.lock is missing, unreadable, or not ASCII") from None
    if len(data) > 16384:
        raise ValueError("source.lock is too large")
    result = {}
    for line in data.splitlines():
        if not line.strip() or line.lstrip().startswith("#"):
            continue
        if line.count("=") != 1:
            raise ValueError("source.lock contains a malformed entry")
        key, value = line.split("=")
        if key not in LOCK_KEYS or key in result or value != value.strip():
            raise ValueError("source.lock contains an unknown or duplicate entry")
        result[key] = value
    if set(result) != LOCK_KEYS:
        raise ValueError("source.lock is missing required entries")
    if (result["version"] != "4.1.7" or result["release_archive_url"] != ARCHIVE_URL
            or result["signature_url"] != SIGNATURE_URL):
        raise ValueError("source.lock is not the canonical AppArmor 4.1.7 release")
    for key, length in [("archive_sha256", 64), ("trusted_public_key_fingerprint", 40),
                        ("trusted_signing_key_fingerprint", 40), ("source_revision", 40)]:
        if not re.fullmatch(r"[0-9a-fA-F]{%d}" % length, result[key]):
            raise ValueError("source.lock contains an unverified or invalid trust field")
    # The archive signature binds the archive. No git transport lookup proves
    # a signed tag, so revision metadata deliberately makes no such claim.
    if result["source_revision_verification"] != "unverified":
        raise ValueError("signed revision verification is not implemented")
    return result


def run_gpg(arguments, home):
    command = ["gpg", "--no-options", "--homedir", str(home), "--batch",
               "--no-auto-key-retrieve", "--auto-key-locate", "clear",
               "--no-default-keyring", "--keyring", str(Path(home) / "trusted.gpg")]
    try:
        result = subprocess.run(command + arguments, capture_output=True, timeout=30,
                                env={**os.environ, "GNUPGHOME": str(home)}, text=True)
    except (OSError, subprocess.TimeoutExpired):
        raise ValueError("GPG unavailable or timed out") from None
    if result.returncode:
        raise ValueError("GPG rejected public key material or signature")
    return result.stdout


def check_keyring(keyring, primary, signing):
    path = Path(keyring)
    if not path.is_file() or path.stat().st_size > 1024 * 1024:
        raise ValueError("trusted public keyring is missing or too large")
    with tempfile.TemporaryDirectory(prefix="apparmor-keycheck-") as home:
        output = run_gpg(["--with-colons", "--import-options", "show-only", "--import", str(path.resolve())], home)
    primary_keys, subkeys = set(), set()
    current = None
    for line in output.splitlines():
        fields = line.split(":")
        if fields[0] in {"sec", "ssb"}:
            raise ValueError("trusted keyring must contain public keys only")
        if fields[0] in {"pub", "sub"}:
            current = fields[0]
        if fields[0] == "fpr":
            (primary_keys if current == "pub" else subkeys).add(fields[9].upper())
    if primary_keys != {primary.upper()} or signing.upper() not in primary_keys | subkeys:
        raise ValueError("public keyring does not match the locked primary/signing key")


def verify_signature(archive, signature, keyring, primary, signing):
    check_keyring(keyring, primary, signing)
    with tempfile.TemporaryDirectory(prefix="apparmor-signature-") as home:
        run_gpg(["--import", str(Path(keyring).resolve())], home)
        status = run_gpg(["--status-fd", "1", "--verify", str(Path(signature).resolve()),
                          str(Path(archive).resolve())], home)
    signatures = []
    for line in status.splitlines():
        fields = line.split()
        if len(fields) > 1 and fields[1] in {"EXPKEYSIG", "EXPSIG", "REVKEYSIG", "BADSIG", "ERRSIG"}:
            raise ValueError("GPG signature is expired, revoked, or invalid")
        if len(fields) >= 11 and fields[:2] == ["[GNUPG:]", "VALIDSIG"]:
            signatures.append((fields[2].upper(), fields[11].upper() if len(fields) > 11 else fields[2].upper()))
    if signatures != [(signing.upper(), primary.upper())]:
        raise ValueError("signature does not match the locked primary/signing fingerprints")
    return {"signing_fingerprint": signatures[0][0], "primary_fingerprint": signatures[0][1]}


def sha256(path):
    digest = hashlib.sha256()
    with Path(path).open("rb") as stream:
        for chunk in iter(lambda: stream.read(1024 * 1024), b""):
            digest.update(chunk)
    return digest.hexdigest()


def verify_sha256(path, expected):
    if sha256(path) != expected.lower():
        raise ValueError("source archive SHA256 does not match source.lock")


def audit_archive(path):
    top = "apparmor-v4.1.7"
    names, links, members, targets = set(), set(), [], []
    total = 0
    with tarfile.open(path, "r:gz") as archive:
        for item in archive:
            name = PurePosixPath(item.name)
            if (name.is_absolute() or ".." in name.parts or not name.parts or name.parts[0] != top
                    or str(name) in names or not (item.isdir() or item.isfile() or item.issym() or item.islnk())
                    or item.mode & 0o7000):
                raise ValueError("source archive contains unsafe or duplicate members")
            names.add(str(name))
            total += item.size
            if len(names) > 100000 or total > 1024 * 1024 * 1024:
                raise ValueError("source archive exceeds extraction limits")
            if item.issym() or item.islnk():
                target = PurePosixPath(item.linkname)
                if target.is_absolute():
                    raise ValueError("source archive link escapes the source root")
                combined = (name.parent / target) if item.issym() else target
                normalized = os.path.normpath(str(combined))
                if normalized != top and not normalized.startswith(top + "/"):
                    raise ValueError("source archive link escapes the source root")
                links.add(str(name))
                targets.append(combined)
            members.append(name)
    for name in members:
        if any(str(parent) in links for parent in name.parents):
            raise ValueError("source archive member traverses a link")
    for target in targets:
        walked = []
        for component in target.parts:
            if component == "..":
                walked.pop()
            else:
                walked.append(component)
            if "/".join(walked) in links:
                raise ValueError("source archive link target traverses another link")
    if not names:
        raise ValueError("source archive is empty")
    return top
