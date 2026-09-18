import json
import subprocess
import sys
import tempfile
import unittest
from pathlib import Path


CONVERTER = Path(__file__).parents[1] / "contrib" / "curl-to-envelope.py"
REQUIRED = {
    "SID": "sid-test",
    "HSID": "hsid-test",
    "SSID": "ssid-test",
    "OSID": "osid-test",
    "APISID": "apisid-test",
    "SAPISID": "sapisid-test",
}


class ConverterTests(unittest.TestCase):
    def run_converter(self, content: str) -> subprocess.CompletedProcess[str]:
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / "cookies.txt"
            path.write_text(content, encoding="utf-8")
            return subprocess.run(
                [sys.executable, str(CONVERTER), str(path)],
                capture_output=True,
                text=True,
                check=False,
            )

    def assert_envelope(self, content: str, expected: dict[str, str]) -> None:
        result = self.run_converter(content)
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(json.loads(result.stdout), {"cookies": expected})
        self.assertEqual(result.stderr, "")

    def test_uppercase_cookie_header(self) -> None:
        header = "; ".join(f"{key}={value}" for key, value in REQUIRED.items())
        self.assert_envelope(f"-H 'Cookie: {header}'", REQUIRED)

    def test_lowercase_cookie_header(self) -> None:
        header = "; ".join(f"{key}={value}" for key, value in REQUIRED.items())
        self.assert_envelope(f'-H "cookie: {header}"', REQUIRED)

    def test_raw_cookie_string(self) -> None:
        header = "; ".join(f"{key}={value}" for key, value in REQUIRED.items())
        self.assert_envelope(header, REQUIRED)

    def test_additional_cookies_are_preserved(self) -> None:
        expected = {**REQUIRED, "NID": "additional-test", "CUSTOM": "kept-test"}
        header = "; ".join(f"{key}={value}" for key, value in expected.items())
        self.assert_envelope(f"--header='Cookie: {header}'", expected)

    def test_missing_required_cookies_are_rejected_without_values(self) -> None:
        content = "Cookie: SID=secret-sid; HSID=secret-hsid"
        result = self.run_converter(content)
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("SSID", result.stderr)
        self.assertIn("SAPISID", result.stderr)
        self.assertNotIn("secret-sid", result.stderr)
        self.assertNotIn("secret-hsid", result.stderr)
        self.assertNotIn("secret-sid", result.stdout)

    def test_malformed_input_is_rejected_without_values(self) -> None:
        content = "this is not a cookie header: secret-value"
        result = self.run_converter(content)
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("no cookies", result.stderr)
        self.assertNotIn("secret-value", result.stderr)
        self.assertNotIn("secret-value", result.stdout)


if __name__ == "__main__":
    unittest.main()
