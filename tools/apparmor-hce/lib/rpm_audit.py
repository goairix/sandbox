"""Inspect RPM headers/payload before any package transaction."""
from pathlib import Path
import re
import stat
import subprocess
import xml.etree.ElementTree as ET

PACKAGE = "sandbox-apparmor-parser"
LICENSE_DIR = "/usr/share/licenses/" + PACKAGE


def command(args):
    try:
        result = subprocess.run(args, capture_output=True, text=True, timeout=120)
    except (OSError, subprocess.TimeoutExpired):
        raise ValueError("RPM audit command unavailable or timed out") from None
    if result.returncode:
        raise ValueError("RPM audit command failed")
    return result.stdout


def reject_scripts(tags):
    for name, values in tags.items():
        upper = name.upper()
        is_script = (re.match(r"^(PREIN|POSTIN|PREUN|POSTUN|PRETRANS|POSTTRANS|PREUNTRANS|POSTUNTRANS|VERIFYSCRIPT)", upper)
                     or "TRIGGER" in upper or "SCRIPT" in upper)
        if not isinstance(values, list):
            values = [values]
        if is_script and any(str(value) not in {"", "(none)"} for value in values):
            raise ValueError("RPM contains a scriptlet or trigger")


def validate_payload(payload):
    seen = set()
    required = {"/usr/sbin/apparmor_parser", LICENSE_DIR + "/COPYING.GPL"}
    allowed = required | {LICENSE_DIR, LICENSE_DIR + "/COPYING.LGPL"}
    for item in payload:
        path, mode = item["path"], item["mode"]
        if path not in allowed or path in seen:
            raise ValueError("RPM payload contains an unexpected or duplicate path")
        seen.add(path)
        expected_mode = 0o40755 if path == LICENSE_DIR else (0o100755 if path == "/usr/sbin/apparmor_parser" else 0o100644)
        if (mode != expected_mode or item["user"] != "root" or item["group"] != "root"
                or item.get("caps", "") or item.get("link", "")):
            raise ValueError("RPM payload has unsafe type, ownership, permissions, capabilities, or links")
    if not required <= seen:
        raise ValueError("RPM payload is missing parser or license")


def headers(path):
    root = ET.fromstring(command(["rpm", "-qp", "--xml", str(path)]))
    tags = {}
    for tag in root.iter("rpmTag"):
        key = tag.attrib["name"].upper()
        if key in tags:
            raise ValueError("RPM has duplicate header tags")
        tags[key] = [child.text or "" for child in tag]
    return tags


def audit_rpm(path, arch, source=False):
    tags = headers(path)
    reject_scripts(tags)
    scalar = lambda key: tags.get(key, [""])[0]
    expected_arch = "src" if source else {"amd64": "x86_64", "arm64": "aarch64"}[arch]
    # Source RPM headers conventionally use the target architecture. The
    # source-package marker is authoritative, together with its .src.rpm name.
    if source:
        if (scalar("SOURCEPACKAGE") != "1" or not Path(path).name.endswith(".src.rpm")
                or scalar("ARCH") not in {"src", {"amd64": "x86_64", "arm64": "aarch64"}[arch]}):
            raise ValueError("source RPM marker is missing")
    elif scalar("ARCH") != expected_arch:
        raise ValueError("binary RPM architecture is incorrect")
    if scalar("NAME") != PACKAGE or scalar("VERSION") != "4.1.7" or scalar("RELEASE") != "1":
        raise ValueError("RPM name/version/release does not match packaging contract")
    # These are deliberately unsigned testing artifacts. Never silently turn
    # a signed/unverified package into a production-valid claim.
    if any(tags.get(key) for key in ("RSAHEADER", "DSAHEADER", "SIGGPG", "SIGPGP")):
        raise ValueError("expected explicitly unsigned testing RPM")
    rows = command(["rpm", "-qp", "--queryformat",
                    "[%{FILENAMES}\t%{FILEMODES}\t%{FILEUSERNAME}\t%{FILEGROUPNAME}\t%{FILELINKTOS}\t%{FILECAPS}\n]", str(path)])
    payload = []
    for row in rows.splitlines():
        fields = row.split("\t")
        if len(fields) != 6:
            raise ValueError("RPM payload metadata is malformed")
        name, mode, user, group, link, caps = fields
        payload.append(dict(path=name, mode=int(mode), user=user, group=group,
                            link=link if link != "(none)" else "", caps=caps if caps != "(none)" else ""))
    if source:
        expected = {"apparmor-v4.1.7.tar.gz", "sandbox-apparmor-parser.spec"}
        if {item["path"] for item in payload} != expected or len(payload) != 2:
            raise ValueError("source RPM contains unexpected or duplicate inputs")
        if any(item["mode"] != 0o100644 or item["user"] != "root" or item["group"] != "root"
               or item["link"] or item["caps"] for item in payload):
            raise ValueError("source RPM has unsafe payload metadata")
    else:
        validate_payload(payload)
    return {"name": scalar("NAME"), "version": scalar("VERSION"), "release": scalar("RELEASE"),
            "arch": scalar("ARCH"), "source": source, "payload": payload, "signature_status": "unsigned-testing-only"}
