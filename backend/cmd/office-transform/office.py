#!/usr/bin/python3
"""Fixed stdin/stdout protocol. Only runs inside the Office profile."""
import json
import os
from pathlib import Path, PurePosixPath
import re
import stat
import struct
import subprocess
import sys
import time
from uuid import uuid4
from urllib.parse import unquote, urlsplit
import zipfile
from xml.etree.ElementTree import ParseError
from xml.sax.saxutils import escape

from defusedxml import ElementTree
from defusedxml.common import DefusedXmlException
import olefile
import uno
import unohelper
from com.sun.star.task import XInteractionHandler

MAX_INPUT = 32 << 20
MAX_EXPANDED = 96 << 20
MAX_XML = 16 << 20
ROOT = Path("/work")
OOXML = {
    "word/document.xml": ("docx", "wordprocessingml.document", "application/vnd.openxmlformats-officedocument.wordprocessingml.document.main+xml"),
    "xl/workbook.xml": ("xlsx", "spreadsheetml.sheet", "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet.main+xml"),
    "ppt/presentation.xml": ("pptx", "presentationml.presentation", "application/vnd.openxmlformats-officedocument.presentationml.presentation.main+xml"),
}
ODF = {
    "application/vnd.oasis.opendocument." + kind: extension
    for kind, extension in (("text", "odt"), ("spreadsheet", "ods"), ("presentation", "odp"))
}
LEGACY = {
    "worddocument": ("doc", "application/msword"),
    "workbook": ("xls", "application/vnd.ms-excel"),
    "book": ("xls", "application/vnd.ms-excel"),
    "powerpoint document": ("ppt", "application/vnd.ms-powerpoint"),
}
FILTERS = {
    "doc": ("MS Word 97", "writer_pdf_Export"),
    "docx": ("Office Open XML Text", "writer_pdf_Export"),
    "xls": ("MS Excel 97", "calc_pdf_Export"),
    "xlsx": ("Calc MS Excel 2007 XML", "calc_pdf_Export"),
    "ppt": ("MS PowerPoint 97", "impress_pdf_Export"),
    "pptx": ("Impress MS PowerPoint 2007 XML", "impress_pdf_Export"),
    "odt": ("writer8", "writer_pdf_Export"),
    "ods": ("calc8", "calc_pdf_Export"),
    "odp": ("impress8", "impress_pdf_Export"),
    "txt": ("OpenDocument Text Flat XML", "writer_pdf_Export"),
}


class InvalidFile(Exception):
    pass


def require(condition):
    if not condition:
        raise InvalidFile


def safe_fields(value):
    return not re.search(r"(?i)\b(?:dde|ddeauto|webservice|include(?:text|picture)|external)\b|(?:https?|file|smb|ftp):[/\\]|\[[^]]+\][^!]+!", value)


def inspect_xml(archive, item, paths):
    require(item.file_size <= MAX_XML)
    depth = 0
    with archive.open(item) as stream:
        events = ElementTree.iterparse(stream, events=("start", "end"), forbid_dtd=True)
        for event, element in events:
            tag = element.tag.rsplit("}", 1)[-1].lower()
            if event == "end":
                if tag in {"f", "formula", "instrtext"}:
                    require(safe_fields(element.text or ""))
                depth -= 1
                element.clear()
                continue
            depth += 1
            require(depth <= 64 and len(element.attrib) <= 128)
            require(not element.tag.startswith("{urn:oasis:names:tc:opendocument:xmlns:script:"))
            require(tag not in {"script", "scripteventlistener", "event-listener", "encryption-data", "datalink", "ddelink",
                                "dde-link", "oleobject", "object-ole", "applet", "plugin",
                                "iframe", "connection", "externalreference"})
            for name, value in element.attrib.items():
                name = name.rsplit("}", 1)[-1].lower()
                if name in {"formula", "instr"}:
                    require(safe_fields(value))
                if name == "targetmode":
                    require(value.lower() == "internal")
                if name not in {"target", "href", "src"}:
                    continue
                decoded = unquote(value).strip()
                uri = urlsplit(decoded)
                require(not uri.scheme and not uri.netloc and not uri.query and "\\" not in decoded
                        and not re.search(r"[\x00-\x1f\x7f]", decoded))
                if name == "target":
                    parent = PurePosixPath(item.filename).parent.parent
                else:
                    parent = PurePosixPath(item.filename).parent
                    require(not uri.path.startswith("/"))
                if uri.path:
                    normalized = os.path.normpath(str(parent / uri.path) if not uri.path.startswith("/") else uri.path.lstrip("/"))
                    require(normalized in paths)


def zip_format(data):
    with zipfile.ZipFile(data) as archive:
        entries = archive.infolist()
        require(0 < len(entries) <= 2048)
        names = set()
        expanded = 0
        for item in entries:
            path = item.filename.rstrip("/")
            lower = path.lower()
            require(path and path.isascii() and "\\" not in path and ":" not in path
                    and all(part not in ("", ".", "..") for part in path.split("/"))
                    and lower not in names and not item.flag_bits & 1
                    and stat.S_IFMT(item.external_attr >> 16) in (0, stat.S_IFREG, stat.S_IFDIR)
                    and item.compress_type in (zipfile.ZIP_STORED, zipfile.ZIP_DEFLATED)
                    and item.file_size <= MAX_INPUT
                    and item.file_size <= max(1 << 20, 200 * item.compress_size))
            require(not any(part.startswith(("vba", "macros", "activex", "customui", "ddelink"))
                            or part in {"basic", "scripts", "embeddings", "externallinks",
                                        "connections.xml", "querytables"}
                            for part in lower.split("/")))
            require(not lower.endswith((".bin", ".exe", ".dll", ".zip", ".docm", ".xlsm", ".pptm", ".svg", ".ttf", ".otf", ".woff", ".woff2", ".odttf")))
            names.add(lower)
            expanded += item.file_size
        require(expanded <= MAX_EXPANDED)
        paths = set(archive.namelist())
        for item in entries:
            if item.is_dir():
                continue
            if item.filename.lower().endswith((".xml", ".rels")):
                inspect_xml(archive, item, paths)
            else:
                with archive.open(item) as stream:
                    signature = stream.read(8)
                    require(not signature.startswith((b"PK\x03\x04", b"\xd0\xcf\x11\xe0\xa1\xb1\x1a\xe1")))
                    while stream.read(64 << 10):
                        pass
        if "mimetype" in paths:
            item = archive.getinfo("mimetype")
            require(item.file_size <= 128 and item.compress_type == zipfile.ZIP_STORED
                    and {"content.xml", "META-INF/manifest.xml"} <= paths
                    and "[Content_Types].xml" not in paths)
            mime = archive.read(item).decode("ascii")
            require(mime in ODF)
            manifest = ElementTree.fromstring(archive.read("META-INF/manifest.xml"), forbid_dtd=True)
            ns = "{urn:oasis:names:tc:opendocument:xmlns:manifest:1.0}"
            require(manifest.tag == ns + "manifest" and any(
                node.attrib.get(ns + "full-path") == "/" and node.attrib.get(ns + "media-type") == mime
                for node in manifest
            ))
            with archive.open("content.xml") as content:
                _, root = next(ElementTree.iterparse(content, events=("start",), forbid_dtd=True))
                require(root.tag == "{urn:oasis:names:tc:opendocument:xmlns:office:1.0}document-content")
            return ODF[mime], mime
        roots = paths & OOXML.keys()
        require(len(roots) == 1 and {"[Content_Types].xml", "_rels/.rels"} <= paths)
        root = roots.pop()
        extension, suffix, content_type = OOXML[root]
        manifest = ElementTree.fromstring(archive.read("[Content_Types].xml"), forbid_dtd=True)
        require(any(node.attrib.get("PartName") == "/" + root
                    and node.attrib.get("ContentType") == content_type for node in manifest))
        for node in manifest:
            kind = node.attrib.get("ContentType", "").lower()
            require(not any(token in kind for token in ("macro", "activex", "oleobject", "vbaproject")))
        relations = ElementTree.fromstring(archive.read("_rels/.rels"), forbid_dtd=True)
        require(any(node.attrib.get("Type", "").endswith("/officeDocument")
                    and node.attrib.get("Target", "").lstrip("/") == root for node in relations))
        return extension, "application/vnd.openxmlformats-officedocument." + suffix


def check_biff(book):
    offset = 0
    while offset < len(book):
        require(len(book) - offset >= 4)
        record, length = struct.unpack_from("<HH", book, offset)
        offset += 4
        require(length <= len(book) - offset and record not in {0x002F, 0x00D3, 0x01AE, 0x0023})
        if record == 0x0085:
            require(length >= 7 and book[offset + 5] not in (1, 6))
        if record in (0x0009, 0x0209, 0x0409, 0x0809):
            require(length >= 4 and book[offset + 2:offset + 4] != b"\x40\x00")
        offset += length


def ole_format(data):
    with olefile.OleFileIO(data, raise_defects=olefile.DEFECT_INCORRECT) as container:
        entries = container.listdir(streams=True, storages=True)
        require(len(entries) <= 128)
        names = {parts[0].lower(): parts[0] for parts in entries if len(parts) == 1}
        require(all(len(parts) == 1 for parts in entries))
        require(not names.keys() & {"encryptedpackage", "encryptioninfo", "encryptedsummary", "objectpool",
                                   "macros", "vba", "_vba_project", "project", "projectwm", "ole10native"})
        roots = names.keys() & LEGACY.keys()
        require(len(roots) == 1)
        root = roots.pop()
        if root == "worddocument":
            with container.openstream(names[root]) as stream:
                header = stream.read(32)
            require(len(header) == 32 and header[:2] == b"\xec\xa5" and not struct.unpack_from("<H", header, 10)[0] & 0x8100)
        if root in {"workbook", "book"}:
            with container.openstream(names[root]) as stream:
                check_biff(stream.read(MAX_INPUT))
        return LEGACY[root]


def file_format(path, first):
    if first.startswith(b"PK\x03\x04"):
        return zip_format(path)
    if first.startswith(b"\xd0\xcf\x11\xe0\xa1\xb1\x1a\xe1"):
        return ole_format(str(path))
    text = path.read_text(encoding="utf-8-sig")
    require(bool(text.strip()) and not re.search(r"[\x00-\x08\x0b\x0c\x0e-\x1f\x7f]", text))
    return "txt", "text/plain"


class AbortInteractions(unohelper.Base, XInteractionHandler):
    def handle(self, request):
        for continuation in request.getContinuations():
            abort = continuation.queryInterface(uno.getTypeByName("com.sun.star.task.XInteractionAbort"))
            if abort is not None:
                abort.select()
                return


def prop(name, value):
    item = uno.createUnoStruct("com.sun.star.beans.PropertyValue")
    item.Name, item.Value = name, value
    return item


def text_document(path, layout):
    page, landscape, margin = layout
    width, height = ("215.9mm", "279.4mm") if page else ("210mm", "297mm")
    if landscape:
        width, height = height, width
    orientation = "landscape" if landscape else "portrait"
    margin = f"{margin or 20}mm"
    source = ROOT / "text.fodt"
    with source.open("w", encoding="utf-8") as out:
        out.write(f"""<?xml version="1.0" encoding="UTF-8"?>
<office:document xmlns:office="urn:oasis:names:tc:opendocument:xmlns:office:1.0"
 xmlns:text="urn:oasis:names:tc:opendocument:xmlns:text:1.0"
 xmlns:style="urn:oasis:names:tc:opendocument:xmlns:style:1.0"
 xmlns:fo="urn:oasis:names:tc:opendocument:xmlns:xsl-fo-compatible:1.0"
 office:version="1.2" office:mimetype="application/vnd.oasis.opendocument.text">
 <office:styles>
  <style:default-style style:family="paragraph"><style:text-properties fo:font-family="Liberation Mono" fo:font-size="11pt"/></style:default-style>
 </office:styles>
 <office:automatic-styles>
  <style:page-layout style:name="Page"><style:page-layout-properties fo:page-width="{width}" fo:page-height="{height}"
     style:print-orientation="{orientation}" fo:margin="{margin}"/></style:page-layout>
 </office:automatic-styles>
 <office:master-styles><style:master-page style:name="Standard" style:page-layout-name="Page"/></office:master-styles>
 <office:body><office:text>
""")
        with path.open(encoding="utf-8-sig") as text:
            for index, line in enumerate(text):
                require(index < 50000)
                line = escape(line.rstrip("\n"))
                line = re.sub(r" +", lambda match: f'<text:s text:c="{len(match[0])}"/>', line)
                out.write("<text:p>" + line.replace("\t", "<text:tab/>") + "</text:p>\n")
                require(out.tell() <= MAX_EXPANDED)
        out.write("</office:text></office:body></office:document>")
    return source


def convert(path, extension, layout):
    if extension == "txt":
        path = text_document(path, layout)
    profile = ROOT / "profile"
    os.environ["HOME"] = str(ROOT)
    os.environ["SAL_USE_VCLPLUGIN"] = "svp"
    pipe = "autolife-office-" + uuid4().hex
    office = subprocess.Popen(
        ["/usr/bin/soffice", "--headless", "--nologo", "--nodefault", "--norestore", "--nofirststartwizard",
         "-env:UserInstallation=" + profile.as_uri(), "--accept=pipe,name=" + pipe + ";urp;StarOffice.ComponentContext"],
        stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL,
    )
    try:
        local = uno.getComponentContext()
        resolver = local.ServiceManager.createInstanceWithContext("com.sun.star.bridge.UnoUrlResolver", local)
        for _ in range(100):
            try:
                remote = resolver.resolve("uno:pipe,name=" + pipe + ";urp;StarOffice.ComponentContext")
                break
            except Exception:
                if office.poll() is not None:
                    raise RuntimeError("Office process failed")
                time.sleep(0.05)
        else:
            raise RuntimeError("Office startup timeout")
        desktop = remote.ServiceManager.createInstanceWithContext("com.sun.star.frame.Desktop", remote)
        options = [
            prop("Hidden", True), prop("ReadOnly", True), prop("FilterName", FILTERS[extension][0]),
            prop("MacroExecutionMode", uno.getConstantByName("com.sun.star.document.MacroExecMode.NEVER_EXECUTE")),
            prop("UpdateDocMode", uno.getConstantByName("com.sun.star.document.UpdateDocMode.NO_UPDATE")),
            prop("InteractionHandler", AbortInteractions()),
        ]
        doc = desktop.loadComponentFromURL(path.as_uri(), "_blank", 0, tuple(options))
        require(doc is not None)
        try:
            filters = tuple(prop(key, value) for key, value in (
                ("ExportFormFields", False), ("ExportBookmarks", False), ("ExportNotes", False),
                ("ExportNotesPages", False), ("IsAddStream", False), ("ExportLinksRelativeFsys", False),
            ))
            doc.storeToURL((ROOT / "raw.pdf").as_uri(),
                           (prop("FilterName", FILTERS[extension][1]),
                            prop("FilterData", uno.Any("[]com.sun.star.beans.PropertyValue", filters))))
        finally:
            doc.dispose()
        desktop.terminate()
    finally:
        try:
            office.wait(timeout=2)
        except subprocess.TimeoutExpired:
            office.kill()
            office.wait()
    raw = ROOT / "raw.pdf"
    output = ROOT / "result.pdf"
    require(raw.is_file() and 5 <= raw.stat().st_size <= MAX_INPUT)
    command = ["qpdf", "--empty", "--pages", str(raw), "--", str(output)]
    require(subprocess.run(command, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL).returncode == 0)
    require(5 <= output.stat().st_size <= MAX_INPUT)
    require(subprocess.run(["/usr/local/bin/check-office-pdf", str(output)], stdout=subprocess.DEVNULL).returncode == 0)
    with output.open("rb") as result:
        while block := result.read(64 << 10):
            sys.stdout.buffer.write(block)


def main():
    mode = sys.stdin.buffer.read(1)
    require(mode in (b"I", b"C"))
    layout = sys.stdin.buffer.read(3) if mode == b"C" else b""
    if mode == b"C":
        require(len(layout) == 3 and layout[0] <= 1 and layout[1] <= 1
                and (layout[2] == 0 or 5 <= layout[2] <= 30))
    data = sys.stdin.buffer.read(MAX_INPUT + 1)
    require(0 < len(data) <= MAX_INPUT)
    path = ROOT / "incoming"
    path.write_bytes(data)
    extension, mime = file_format(path, data[:8])
    if mode == b"I":
        print(json.dumps({"mime": mime}))
        return
    source = ROOT / ("input." + extension)
    path.rename(source)
    require(extension == "txt" or layout == b"\x00\x00\x00")
    convert(source, extension, layout)


if __name__ == "__main__":
    try:
        main()
    except (InvalidFile, ValueError, OSError, ParseError, DefusedXmlException, zipfile.BadZipFile):
        sys.exit(20)
