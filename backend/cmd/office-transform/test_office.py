"""Native smoke fixtures. Run explicitly with the Office image and isolated /work."""
import io
import json
from pathlib import Path
import shutil
import subprocess
import unittest
import zipfile

ROOT = Path("/work")
CONVERTER = "/usr/local/bin/autolife-office"
OFFICE = "/usr/bin/soffice"

DOCUMENT = """<?xml version="1.0" encoding="UTF-8"?>
<office:document xmlns:office="urn:oasis:names:tc:opendocument:xmlns:office:1.0"
 xmlns:text="urn:oasis:names:tc:opendocument:xmlns:text:1.0"
 xmlns:table="urn:oasis:names:tc:opendocument:xmlns:table:1.0"
 xmlns:draw="urn:oasis:names:tc:opendocument:xmlns:drawing:1.0"
 xmlns:svg="urn:oasis:names:tc:opendocument:xmlns:svg-compatible:1.0"
 office:version="1.2" office:mimetype="application/vnd.oasis.opendocument.{}">
 <office:body><office:{}>{}</office:{}></office:body>
</office:document>"""
SAMPLE = "AutoLife Привет мир"


class OfficeSmoke(unittest.TestCase):
    def invoke(self, mode, payload):
        for name in ("incoming", "raw.pdf", "result.pdf", "profile", "text.fodt"):
            path = ROOT / name
            if path.is_dir():
                shutil.rmtree(path)
            elif path.exists():
                path.unlink()
        for pattern in ("input.*", "OSL_PIPE_65534_*"):
            for path in ROOT.glob(pattern):
                path.unlink()
        return subprocess.run([CONVERTER], input=mode + payload, capture_output=True, timeout=35)

    def test_formats(self):
        specs = [
            ("text", "fodt", "<text:p>" + SAMPLE + "</text:p>",
             [("doc", "MS Word 97"), ("docx", "Office Open XML Text"), ("odt", "writer8")]),
            ("spreadsheet", "fods", '<table:table table:name="Sample"><table:table-row><table:table-cell office:value-type="string"><text:p>'
             + SAMPLE + "</text:p></table:table-cell></table:table-row></table:table>",
             [("xls", "MS Excel 97"), ("xlsx", "Calc MS Excel 2007 XML"), ("ods", "calc8")]),
            ("presentation", "fodp", '<draw:page draw:name="First"><draw:frame svg:x="1cm" svg:y="1cm" svg:width="10cm" svg:height="2cm">'
             "<draw:text-box><text:p>" + SAMPLE + "</text:p></draw:text-box></draw:frame></draw:page>",
             [("ppt", "MS PowerPoint 97"), ("pptx", "Impress MS PowerPoint 2007 XML"), ("odp", "impress8")]),
        ]
        for kind, extension, content, outputs in specs:
            sample = ROOT / ("sample." + extension)
            sample.write_text(DOCUMENT.format(kind, kind, content, kind), encoding="utf-8")
            for output, output_filter in outputs:
                with self.subTest(extension=output):
                    command = [OFFICE, "-env:UserInstallation=file:///work/fixtures", "--headless",
                               "--convert-to", output + ":" + output_filter, "--outdir", str(ROOT), str(sample)]
                    subprocess.run(command, check=True, capture_output=True, timeout=20)
                    data = (ROOT / ("sample." + output)).read_bytes()
                    self.assert_pdf(data)
    def test_text_geometry(self):
        self.assert_pdf((SAMPLE + "\n").encode("utf-8-sig"), b"C\x01\x01\x0f")
        info = subprocess.run(["pdfinfo", "/work/result.pdf"], check=True, capture_output=True)
        self.assertIn(b"792 x 612 pts", info.stdout)

    def assert_pdf(self, data, header=b"C\x00\x00\x00"):
        result = self.invoke(b"I", data)
        self.assertEqual(0, result.returncode, result.stderr)
        self.assertIn("mime", json.loads(result.stdout))
        result = self.invoke(header, data)
        self.assertEqual(0, result.returncode, result.stderr)
        self.assertTrue(result.stdout.startswith(b"%PDF-"))
        text = subprocess.run(["pdftotext", "/work/result.pdf", "-"], check=True, capture_output=True)
        self.assertIn(SAMPLE, text.stdout.decode("utf-8"))

    def test_rejects_invalid_inputs(self):
        for payload in (b"\x00broken", b"PK\x03\x04broken", b"\xd0\xcf\x11\xe0\xa1\xb1\x1a\xe1broken"):
            with self.subTest(payload=payload):
                result = self.invoke(b"I", payload)
                self.assertEqual(20, result.returncode, result.stderr)
        for bad_part in ("../escape", "word/vbaProject.bin", "Object 1/evil.svg"):
            archive = io.BytesIO()
            with zipfile.ZipFile(archive, "w") as target:
                target.writestr("mimetype", "application/vnd.oasis.opendocument.text")
                target.writestr("content.xml", "<document/>")
                target.writestr("META-INF/manifest.xml", "<manifest/>")
                target.writestr(bad_part, "content")
            result = self.invoke(b"I", archive.getvalue())
            self.assertEqual(20, result.returncode, result.stderr)
        for xml in ('<doc href="file:///etc/passwd"/>', '<doc href="https://example.invalid/"/>',
                    '<!DOCTYPE doc [<!ENTITY value "unsafe">]><doc>&value;</doc>', '<doc><encryption-data/></doc>'):
            archive = io.BytesIO()
            with zipfile.ZipFile(archive, "w") as target:
                target.writestr("mimetype", "application/vnd.oasis.opendocument.text")
                target.writestr("content.xml", xml)
                target.writestr("META-INF/manifest.xml", "<manifest/>")
            result = self.invoke(b"I", archive.getvalue())
            self.assertEqual(20, result.returncode, result.stderr)


if __name__ == "__main__":
    unittest.main(verbosity=2)
