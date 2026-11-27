"""Functional checks for the shipped native binary, separate from TASK-010 confinement."""
import io
import os
import random
import struct
import subprocess

from PIL import Image, ImageCms

IMAGE = os.getenv("AUTOLIFE_IMAGE_TEST_TAG", "autolife-task006-image")


def fixture(fmt, mode="RGB", size=(80, 40), color=(200, 20, 10), **args):
    buf = io.BytesIO()
    Image.new(mode, size, color).save(buf, fmt, **args)
    return buf.getvalue()


def transform(data, target=0, quality=85, flags=0, box=(0, 0), bg=(255, 255, 255), expected=0):
    command = [
        "docker", "run", "--rm", "-i", "--network", "none", "--read-only",
        "--cap-drop", "ALL", "--security-opt", "no-new-privileges", "--ipc", "none",
        "--user", "65534:65534", "--cpus", "1", "--memory", "512m", "--memory-swap", "512m",
        "--pids-limit", "32", "--tmpfs", "/work:rw,noexec,nosuid,nodev,size=384m,mode=1777",
        "--env", "TMPDIR=/work", IMAGE,
    ]
    header = struct.pack(">4sBBBBBBII", b"ALI1", target, quality, flags, *bg, *box)
    result = subprocess.run(command, input=header + data, capture_output=True, timeout=30)
    assert result.returncode == expected, (result.returncode, expected, result.stderr.decode(errors="replace"))
    if expected:
        assert not result.stdout
    return result.stdout


def decoded(data):
    image = Image.open(io.BytesIO(data))
    image.load()
    return image


formats = ("JPEG", "PNG", "WEBP")
for source in formats:
    data = fixture(source)
    for target, name in enumerate(formats, 1):
        image = decoded(transform(data, target))
        assert image.format == name and image.size == (80, 40), (source, name, image.size)
print("PASS: all nine baseline format pairs")

alpha = fixture("PNG", "RGBA", color=(200, 20, 100, 0))
for target in (2, 3):
    im = decoded(transform(alpha, target))
    assert im.convert("RGBA").getpixel((20, 20))[3] == 0
jpeg = decoded(transform(alpha, 1, bg=(80, 140, 210)))
assert all(abs(v - expect) <= 3 for v, expect in zip(jpeg.getpixel((20, 20)), (80, 140, 210)))
print("PASS: PNG/WebP alpha, JPEG background")

exif = Image.Exif()
exif[274] = 6
exif[271] = "private metadata fixture"
exif[34853] = {1: "N", 2: (1.0, 2.0, 3.0), 3: "E", 4: (4.0, 5.0, 6.0)}
icc = ImageCms.ImageCmsProfile(ImageCms.createProfile("sRGB")).tobytes()
original = fixture("JPEG", exif=exif, icc_profile=icc)
oriented = decoded(transform(original, 2))
assert oriented.size == (40, 80)
assert not oriented.getexif() and not oriented.info.get("icc_profile")
assert decoded(original).getexif().get(271) == "private metadata fixture"
assert decoded(original).getexif().get_ifd(34853)[1] == "N"
print("PASS: EXIF autorotation, embedded ICC and stripping of GPS/EXIF")

for source, size, flags, box, target in (
    (fixture("PNG"), (25, 13), 10, (25, 25), 2),
    (fixture("PNG", size=(40, 80)), (13, 25), 10, (25, 25), 2),
    (alpha, (80, 40), 10, (100, 100), 2),
    (alpha, (160, 80), 14, (160, 90), 2),
    (alpha, (25, 15), 8, (25, 15), 2),
):
    im = decoded(transform(source, target, flags=flags, box=box))
    assert im.size == size, (size, im.size)
    if decoded(source).mode == "RGBA":
        assert im.getpixel((0, 0))[3] == 0
print("PASS: landscape, portrait, alpha, fit, stretch and upscale")

noise = Image.frombytes("RGB", (128, 128), random.Random(8).randbytes(128 * 128 * 3))
buffer = io.BytesIO()
noise.save(buffer, "PNG")
data = buffer.getvalue()
assert len(transform(data, 1, quality=20)) < len(transform(data, 1, quality=95))
assert decoded(transform(data, 3, flags=1)).tobytes() == noise.tobytes()
assert decoded(transform(data, 2, flags=1)).tobytes() == noise.tobytes()
transform(fixture("JPEG"), flags=1, expected=20)
transform(data, quality=101, expected=20)
print("PASS: quality effect, lossless WebP/PNG, invalid quality and JPEG mode")

transform(b'<svg xmlns="http://www.w3.org/2000/svg"></svg>', target=2, expected=20)
transform(fixture("PNG")[:35], target=2, expected=20)
transform(fixture("PNG", "L", (8000, 5001), 0), target=2, expected=20)
transform(fixture("PNG"), target=2, flags=14, box=(40000, 40000), expected=20)
print("PASS: unsupported, malformed and oversized inputs/outputs")
