#!/usr/bin/env python3
"""Exercise the installer with local HTTP assets; never contact GitHub."""
import hashlib
import http.server
import io
import os
import pathlib
import shutil
import subprocess
import tarfile
import tempfile
import threading
import unittest

ROOT = pathlib.Path(__file__).resolve().parents[1]
INSTALLER = ROOT / "docker/images/common/install-ripgrep-14.1.1.sh"
ASSETS = {
    "amd64": ("ripgrep-14.1.1-x86_64-unknown-linux-musl", "4cf9f2741e6c465ffdb7c26f38056a59e2a2544b51f7cc128ef28337eeae4d8e"),
    "arm64": ("ripgrep-14.1.1-aarch64-unknown-linux-gnu", "c827481c4ff4ea10c9dc7a4022c8de5db34a5737cb74484d62eb94a95841ab2f"),
}


class Handler(http.server.BaseHTTPRequestHandler):
    def do_GET(self):
        self.server.requests.append(self.path)
        self.send_response(self.server.status)
        self.end_headers()
        self.wfile.write(self.server.payload)

    def log_message(self, *_):
        pass


class InstallerTest(unittest.TestCase):
    def run_installer(self, arch, status=200, corrupt=False, pin_fixture=True):
        directory, production_hash = ASSETS[arch]
        binary = b"#!/bin/sh\nprintf 'ripgrep 14.1.1 test fixture\\n'\n"
        archive = io.BytesIO()
        with tarfile.open(fileobj=archive, mode="w:gz") as stream:
            member = tarfile.TarInfo(directory + "/rg")
            member.size = len(binary)
            member.mode = 0o755
            stream.addfile(member, io.BytesIO(binary))
        trusted = archive.getvalue()
        server = http.server.ThreadingHTTPServer(("127.0.0.1", 0), Handler)
        server.requests = []
        server.status = status
        server.payload = b"not the pinned archive" if corrupt else trusted
        thread = threading.Thread(target=server.serve_forever, daemon=True)
        thread.start()
        try:
            with tempfile.TemporaryDirectory(prefix="sandbox-ripgrep-test-") as temp:
                root = pathlib.Path(temp)
                (root / "tmp").mkdir()
                (root / "out").mkdir()
                (root / "bin").mkdir()
                source = INSTALLER.read_text()
                # Private fixture: substitute only the trusted test digest and
                # filesystem roots. Production has no checksum override option.
                if pin_fixture:
                    source = source.replace(production_hash, hashlib.sha256(trusted).hexdigest())
                source = source.replace("/tmp", str(root / "tmp")).replace("/out/rg", str(root / "out/rg"))
                script = root / "installer.sh"
                script.write_text(source)
                wrapper = root / "bin/curl"
                wrapper.write_text("#!/usr/bin/env python3\nimport os, sys\n"
                                   "urls = [v for v in sys.argv[1:] if v.startswith(('http://', 'https://'))]\n"
                                   "if not urls or any(not v.startswith(os.environ['TEST_BASE'] + '/') for v in urls):\n"
                                   "    sys.stderr.write('unexpected external download URL\\n'); sys.exit(22)\n"
                                   "os.execv(os.environ['TEST_CURL'], ['curl', *sys.argv[1:]])\n")
                wrapper.chmod(0o755)
                base = "http://127.0.0.1:" + str(server.server_port) + "/releases/14.1.1"
                environment = {**os.environ, "PATH": str(root / "bin") + os.pathsep + os.environ["PATH"],
                               "TARGETARCH": arch, "RIPGREP_DOWNLOAD_BASE_URL": base + "/",
                               "TEST_BASE": base, "TEST_CURL": shutil.which("curl"),
                               "NO_PROXY": "127.0.0.1", "no_proxy": "127.0.0.1"}
                result = subprocess.run(["sh", str(script)], env=environment, capture_output=True, timeout=12)
                installed = root / "out/rg"
                return result, list(server.requests), installed.read_bytes() if installed.exists() else None, binary
        finally:
            server.shutdown()
            server.server_close()
            thread.join()

    def test_mirror_installs_both_architectures_after_verification(self):
        for arch, (directory, _) in ASSETS.items():
            with self.subTest(arch=arch):
                result, requests, installed, binary = self.run_installer(arch)
                self.assertEqual(result.returncode, 0, result.stderr.decode())
                self.assertEqual(requests, ["/releases/14.1.1/" + directory + ".tar.gz"])
                self.assertEqual(installed, binary)

    def test_corrupt_download_is_never_installed(self):
        result, requests, installed, _ = self.run_installer("amd64", corrupt=True)
        self.assertTrue(requests, "configured mirror was not used")
        self.assertNotEqual(result.returncode, 0)
        self.assertIn(b"checksum", result.stderr.lower())
        self.assertIsNone(installed)

    def test_403_names_the_download_stage_and_configuration(self):
        result, requests, installed, _ = self.run_installer("amd64", status=403)
        self.assertTrue(requests, "configured mirror was not used")
        self.assertNotEqual(result.returncode, 0)
        self.assertIn(b"403", result.stderr)
        self.assertIn(b"RIPGREP_DOWNLOAD_BASE_URL", result.stderr)
        self.assertIsNone(installed)

    def test_production_digest_rejects_an_untrusted_mirror_asset(self):
        result, requests, installed, _ = self.run_installer("arm64", pin_fixture=False)
        self.assertTrue(requests, "configured mirror was not used")
        self.assertNotEqual(result.returncode, 0)
        self.assertIsNone(installed)


if __name__ == "__main__":
    unittest.main()
