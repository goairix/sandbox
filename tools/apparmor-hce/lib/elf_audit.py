"""ELF64 structure checks; execution and ABI checks happen only in HCE."""
from pathlib import Path
import struct


def inspect_elf(path, arch):
    machines = {"amd64": 62, "arm64": 183}
    interpreters = {"amd64": "/lib64/ld-linux-x86-64.so.2", "arm64": "/lib/ld-linux-aarch64.so.1"}
    data = Path(path).read_bytes()
    if arch not in machines or len(data) < 64 or data[:7] != b"\x7fELF\x02\x01\x01":
        raise ValueError("parser is not a valid little-endian ELF64 executable")
    fields = struct.unpack_from("<HHIQQQIHHHHHH", data, 16)
    kind, machine, version, entry, phoff, shoff, flags, ehsize, phsize, phnum, shsize, shnum, shstr = fields
    if (kind not in {2, 3} or machine != machines[arch] or version != 1 or ehsize != 64
            or phsize != 56 or not 1 <= phnum <= 1024 or phoff < 64
            or phoff + phnum * phsize > len(data)):
        raise ValueError("parser ELF header/program table is invalid or has the wrong architecture")
    if shnum and (shsize != 64 or shoff < 64 or shoff + shnum * shsize > len(data) or shstr >= shnum):
        raise ValueError("parser ELF section table is invalid")
    loads, interpreter, executable_entry = [], [], False
    for index in range(phnum):
        ptype, pflags, offset, vaddr, _, filesz, memsz, align = struct.unpack_from("<IIQQQQQQ", data, phoff + index * phsize)
        if offset + filesz > len(data) or (align > 1 and align & (align - 1)):
            raise ValueError("parser ELF segment is out of bounds or misaligned")
        if ptype == 1:
            if filesz > memsz or (align > 1 and (vaddr - offset) % align):
                raise ValueError("parser ELF load segment is invalid")
            loads.append((vaddr, memsz))
            executable_entry |= bool(pflags & 1 and vaddr <= entry < vaddr + memsz)
        if ptype == 3:
            raw = data[offset:offset + filesz]
            if not raw.endswith(b"\0") or b"\0" in raw[:-1]:
                raise ValueError("parser ELF interpreter is malformed")
            try:
                interpreter.append(raw[:-1].decode("ascii"))
            except UnicodeError:
                raise ValueError("parser ELF interpreter is not ASCII") from None
    if not loads or not executable_entry or interpreter != [interpreters[arch]]:
        raise ValueError("parser ELF entry point or interpreter is invalid")
    return {"class": "ELF64", "machine": machine, "type": kind, "interpreter": interpreter[0],
            "check_level": "structural-only; not execution or native HCE evidence"}


if __name__ == "__main__":
    import sys
    try:
        inspect_elf(sys.argv[1], {"x86_64": "amd64", "aarch64": "arm64"}.get(sys.argv[2], sys.argv[2]))
    except (ValueError, OSError, IndexError):
        print("error: parser ELF structure or architecture is invalid", file=sys.stderr)
        sys.exit(2)
