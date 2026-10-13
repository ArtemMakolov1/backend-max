import importlib.util
from pathlib import Path
import subprocess
import sys
import unittest

SCRIPT = Path(__file__).resolve().parents[1] / "validate-exa-proxy-url.py"
spec = importlib.util.spec_from_file_location("exa_proxy_validation", SCRIPT)
module = importlib.util.module_from_spec(spec)
spec.loader.exec_module(module)


class ProxyURLPolicyTest(unittest.TestCase):
    def test_explicit_authorities_and_encoded_userinfo(self):
        for value in ["", "http://proxy.example:3128", "https://proxy.example",
                      "http://127.0.0.1:8080", "https://[2001:db8::1]:8443",
                      "http://user:synthetic%24proxy%27password@proxy.example:3128"]:
            with self.subTest(value=value):
                self.assertTrue(module.valid(value))

    def test_invalid_proxy_and_env_file_boundaries(self):
        for value in ["socks5://proxy.example:1080", "file:///tmp/proxy",
                      "http://proxy.example:0", "http://proxy.example:65536",
                      "http://proxy.example:", "http://proxy.example/path",
                      "http://proxy.example?", "http://proxy.example#",
                      "http://proxy.example/#fragment", "http://:secret@proxy.example",
                      "http://user:p%0Asecret@proxy.example", "http://user:p%ZZ@proxy.example",
                      "http://user:p$secret@proxy.example", r"http://user:p\\secret@proxy.example",
                      "http://user^:secret@proxy.example", "http://user:secret`value@proxy.example",
                      "http://bad_label.example", "http://user:synthetic-secret@proxy.example\nINJECTED=secret"]:
            with self.subTest(value=value):
                self.assertFalse(module.valid(value))
        for value in ["http://user:secret@[::1]suffix", "http://user:secret@[::1]suffix:3128"]:
            with self.subTest(value=value):
                self.assertFalse(module.valid(value))

    def test_errors_do_not_echo_proxy_credentials(self):
        for value in [b"http://user:synthetic-proxy-secret@proxy.example/path",
                      b"http://user:synthetic-proxy-secret@proxy.example\nINJECTED=secret",
                      b"x" * 4097, b"\xff"]:
            result = subprocess.run([sys.executable, str(SCRIPT)], input=value, capture_output=True)
            self.assertEqual(result.returncode, 1)
            self.assertEqual(result.stdout, b"")
            self.assertNotIn(b"synthetic-proxy-secret", result.stderr)
            self.assertNotIn(b"proxy.example", result.stderr)


if __name__ == "__main__":
    unittest.main()

