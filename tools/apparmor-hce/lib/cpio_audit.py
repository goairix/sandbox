"""Parse RPM's newc payload without extracting untrusted filesystem entries."""
from pathlib import PurePosixPath
import stat
import subprocess


def parse_newc(data):
    offset, seen, items = 0, set(), []
    while offset + 110 <= len(data):
        header = data[offset:offset + 110]
        if header[:6] != b"070701":
            raise ValueError("RPM payload is not supported newc format")
        try:
            values = [int(header[i:i + 8], 16) for i in range(6, 110, 8)]
        except ValueError:
            raise ValueError("RPM payload has an invalid cpio header") from None
        _, mode, uid, gid, nlink, _, size, _, _, _, _, namesize, check = values
        offset += 110
        if not 1 <= namesize <= 4096 or offset + namesize > len(data):
            raise ValueError("RPM payload cpio name is out of bounds")
        raw_name = data[offset:offset + namesize]
        if raw_name[-1:] != b"\0" or b"\0" in raw_name[:-1]:
            raise ValueError("RPM payload cpio name is invalid")
        try:
            name = raw_name[:-1].decode("utf-8")
        except UnicodeError:
            raise ValueError("RPM payload cpio name is not UTF-8") from None
        offset = (offset + namesize + 3) & ~3
        if offset + size > len(data):
            raise ValueError("RPM payload cpio body is truncated")
        body = data[offset:offset + size]
        offset = (offset + size + 3) & ~3
        if name == "TRAILER!!!":
            if size or any(data[offset:]):
                raise ValueError("RPM payload has data after cpio trailer")
            return items
        path = PurePosixPath(name)
        canonical = str(path)
        if (path.is_absolute() or ".." in path.parts or canonical in {"", "."} or canonical in seen
                or uid or gid or mode & 0o7000 or not (stat.S_ISREG(mode) or stat.S_ISDIR(mode))
                or (stat.S_ISREG(mode) and nlink != 1) or check):
            raise ValueError("RPM payload has unsafe paths, owners, types, modes, or hardlinks")
        seen.add(canonical)
        items.append({"path": canonical, "mode": mode, "data": body})
    raise ValueError("RPM payload lacks a complete cpio trailer")


def read_payload(path, header_payload):
    try:
        result = subprocess.run(["rpm2cpio", str(path)], capture_output=True, timeout=120)
    except (OSError, subprocess.TimeoutExpired):
        raise ValueError("RPM payload reader unavailable or timed out") from None
    if result.returncode or len(result.stdout) > 128 * 1024 * 1024:
        raise ValueError("RPM payload reader failed or payload is too large")
    payload = parse_newc(result.stdout)
    actual = {item["path"]: item["mode"] for item in payload}
    expected = {item["path"].lstrip("/"): item["mode"] for item in header_payload}
    if actual != expected:
        raise ValueError("RPM cpio payload does not match audited header paths/modes")
    return {item["path"]: item["data"] for item in payload}
