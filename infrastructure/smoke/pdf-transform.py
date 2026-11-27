"""Functional checks for the shipped PDF processor; TASK-010 verifies confinement."""
import io
import json
import os
import struct
import subprocess

from PIL import Image
from pypdf import PdfReader, PdfWriter

IMAGE = os.getenv("AUTOLIFE_PDF_TEST_TAG", "autolife-task007-pdf")


def fixture(widths, *, password=None, active=False, attached=False):
    doc = PdfWriter()
    for width in widths:
        doc.add_blank_page(width=width, height=300)
    doc.add_metadata({"/Author": "private fixture"})
    if active:
        doc.open_destination = doc.pages[0]
    if attached:
        doc.add_attachment("sample.txt", b"minimal fixture")
    if password is not None:
        doc.encrypt(password, owner_password="fixture-owner")
    out = io.BytesIO()
    doc.write(out)
    return out.getvalue()


def transform(operation, sources, *, expected=0, **options):
    args = [
        "docker", "run", "--rm", "-i", "--network", "none", "--read-only",
        "--cap-drop", "ALL", "--security-opt", "no-new-privileges", "--ipc", "none",
        "--user", "65534:65534", "--cpus", "1", "--memory", "768m", "--memory-swap", "768m",
        "--pids-limit", "32", "--tmpfs", "/work:rw,noexec,nosuid,nodev,size=768m,mode=1777",
        "--env", "TMPDIR=/work", IMAGE,
    ]
    header = json.dumps(dict(operation=operation, sizes=list(map(len, sources)), **options)).encode()
    run = subprocess.run(args, input=struct.pack(">I", len(header)) + header + b"".join(sources),
                         capture_output=True, timeout=90)
    assert run.returncode == expected, (run.returncode, expected, run.stderr)
    if expected:
        assert not run.stdout
        return []
    body = io.BytesIO(run.stdout)
    count, = struct.unpack(">I", body.read(4))
    results = []
    for _ in range(count):
        size, = struct.unpack(">q", body.read(8))
        item = body.read(size)
        assert len(item) == size
        result = PdfReader(io.BytesIO(item), strict=True)
        assert not result.is_encrypted and not (result.metadata or {}).get("/Author")
        results.append(result)
    assert body.read() == b""
    return results


def widths(doc):
    return [int(page.mediabox.width) for page in doc.pages]


a, b = fixture([110, 120]), fixture([130, 140])
assert widths(transform("merge", [b, a])[0]) == [130, 140, 110, 120]
assert [widths(p) for p in transform("split", [a])] == [[110], [120]]
four = fixture([110, 120, 130, 140])
assert [widths(p) for p in transform("split", [four], ranges=["3-4", "1"])] == [[130, 140], [110]]
assert widths(transform("extract", [four], ranges=["4", "1-2"])[0]) == [140, 110, 120]
for selection in (["5"], ["1-3", "2"], ["2-1"], ["--password=x"], ["0"]):
    transform("extract", [four], ranges=selection, expected=20)
print("PASS: merge, split, extract, order, page bounds and duplicates")

for invalid in (a[:80], fixture([110], password="test"), fixture([110], password=""), fixture([110], active=True), fixture([110], attached=True)):
    transform("merge", [invalid], expected=20)
assert len(transform("merge", [fixture([110] * 499), fixture([110])])[0].pages) == 500
transform("merge", [fixture([110] * 499), fixture([110, 110])], expected=20)
print("PASS: static policy, password and aggregate 500/501-page limit")

images = []
for fmt, size, mode in (("JPEG", (40, 100), "RGB"), ("PNG", (120, 80), "RGBA"), ("WEBP", (90, 90), "RGBA")):
    output = io.BytesIO()
    Image.new(mode, size, (10, 100, 200) if mode == "RGB" else (10, 100, 200, 125)).save(output, fmt)
    images.append(output.getvalue())
pdf, = transform("create", images, page_size="A4", orientation="auto", fit="contain")
assert len(pdf.pages) == 3
assert [(float(p.mediabox.width) > float(p.mediabox.height)) for p in pdf.pages] == [False, True, False]
for page, size in zip(pdf.pages, ((40, 100), (120, 80), (90, 90))):
    assert abs(min(float(page.mediabox.width), float(page.mediabox.height)) - 595.2756) < .1
    image, = page.images
    assert Image.open(io.BytesIO(image.data)).size == size
    matrix, = [operands for operands, op in page.get_contents().operations if op == b"cm"]
    w, h = float(matrix[0]), float(matrix[3])
    assert abs(w / h - size[0] / size[1]) < .001
    assert w <= float(page.mediabox.width) + .01 and h <= float(page.mediabox.height) + .01

oriented, = transform("create", [images[0]], page_size="Letter", orientation="landscape", fit="shrink")
assert oriented.pages[0].mediabox.width == 792 and oriented.pages[0].mediabox.height == 612
autosized, = transform("create", [images[1]], page_size="auto", orientation="portrait", fit="contain")
assert autosized.pages[0].mediabox.width == 60 and autosized.pages[0].mediabox.height == 90
transform("create", [b"<svg/>"], page_size="A4", orientation="auto", fit="contain", expected=20)
print("PASS: JPEG/PNG/WebP, image order, A4/Letter/auto, fit and orientation")
