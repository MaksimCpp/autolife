"""Render static qpdf-verified input. The Go entrypoint supplies the plan."""
from contextlib import ExitStack
import json
import math
from pathlib import Path
import sys

import pypdfium2 as pdfium

MAX_PAGE_PIXELS = 40_000_000
MAX_JOB_PIXELS = 200_000_000
MAX_FILE = 100_000_000
MAX_TOTAL = 250_000_000


def render(plan):
    if {"V8", "XFA"} & set(pdfium.PDFIUM_INFO.flags):
        raise ValueError("incompatible PDFium build")
    scale = plan["DPI"] / 72
    color = bytes.fromhex(plan["Background"][1:])
    fill = tuple(color) + (0 if plan["Alpha"] else 255,)
    with ExitStack() as stack:
        documents = []
        expected_pixels = 0
        for item in plan["Documents"]:
            doc = pdfium.PdfDocument(item["Path"])
            stack.callback(doc.close)
            documents.append((doc, item["Pages"]))
            for number in item["Pages"]:
                page = doc[number - 1]
                try:
                    width, height = page.get_size()
                    width, height = math.ceil(width * scale), math.ceil(height * scale)
                finally:
                    page.close()
                if min(width, height) < 1 or width * height > MAX_PAGE_PIXELS:
                    raise ValueError("page pixel limit")
                expected_pixels += width * height
                if expected_pixels > MAX_JOB_PIXELS:
                    raise ValueError("job pixel limit")

        index = total_bytes = 0
        for doc, pages in documents:
            for number in pages:
                index += 1
                output = Path(f"/work/result-{index:03d}.{plan['Format']}")
                page = doc[number - 1]
                try:
                    bitmap = page.render(scale=scale, fill_color=fill, draw_annots=False)
                    try:
                        image = bitmap.to_pil()
                        try:
                            if plan["Format"] == "jpeg":
                                with image.convert("RGB") as rgb:
                                    rgb.save(output, "JPEG", quality=plan["Quality"])
                            else:
                                image.save(output, "PNG")
                        finally:
                            image.close()
                    finally:
                        bitmap.close()
                finally:
                    page.close()

                size = output.stat().st_size
                total_bytes += size
                if not 0 < size <= MAX_FILE or total_bytes > MAX_TOTAL:
                    raise ValueError("output byte limit")


if __name__ == "__main__":
    render(json.load(sys.stdin))
