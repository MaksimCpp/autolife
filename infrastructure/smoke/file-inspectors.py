import io
import json
import struct
import subprocess
import zlib

from PIL import Image

def pdf(count=1, active=False):
    catalog = '<< /Type /Catalog /Pages 2 0 R' + (' /OpenAction [3 0 R /Fit]' if active else '') + ' >>'
    objects = [catalog, f'<< /Type /Pages /Kids [{" ".join(f"{i+3} 0 R" for i in range(count))}] /Count {count} >>']
    objects += ['<< /Type /Page /Parent 2 0 R /MediaBox [0 0 72 72] /Resources << >> >>'] * count
    result = bytearray(b'%PDF-1.4\n')
    offsets = []
    for i, obj in enumerate(objects, 1):
        offsets.append(len(result))
        result += f'{i} 0 obj\n{obj}\nendobj\n'.encode()
    xref = len(result)
    result += f'xref\n0 {len(objects)+1}\n0000000000 65535 f \n'.encode()
    for offset in offsets:
        result += f'{offset:010d} 00000 n \n'.encode()
    result += f'trailer\n<< /Root 1 0 R /Size {len(objects)+1} >>\nstartxref\n{xref}\n%%EOF\n'.encode()
    return bytes(result)

def run(label, profile, data, expected, expected_mime=None):
    command = ['docker','run','--rm','-i','--network','none','--read-only','--cap-drop','ALL','--security-opt','no-new-privileges','--user','65534:65534','--memory','512m','--pids-limit','32','--tmpfs','/work:rw,noexec,nosuid,nodev,size=128m,mode=1777','--entrypoint','/usr/local/bin/autolife-inspect',f'autolife-task003-{profile}']
    result = subprocess.run(command,input=data,capture_output=True,timeout=30)
    if result.returncode != expected or expected_mime and json.loads(result.stdout)['mime'] != expected_mime:
        raise RuntimeError((label,result.returncode,result.stdout,result.stderr))
    print(label, 'PASS', flush=True)

for fmt in ('PNG','JPEG','WEBP'):
    data=io.BytesIO()
    Image.new('RGB',(16,16)).save(data,fmt)
    run(fmt,'image',data.getvalue(),0,f'image/{fmt.lower()}')
    run(fmt+' truncated','image',data.getvalue()[:20],20)

def chunk(kind,payload):
    return struct.pack('>I',len(payload))+kind+payload+struct.pack('>I',zlib.crc32(kind+payload))
oversized=b'\x89PNG\r\n\x1a\n'+chunk(b'IHDR',struct.pack('>IIBBBBB',8000,5001,8,2,0,0,0))+chunk(b'IDAT',zlib.compress(b'\0'))+chunk(b'IEND',b'')
run('oversized PNG','image',oversized,20)
run('static PDF','pdf',pdf(),0,'application/pdf')
run('static 500-page PDF','pdf',pdf(500),0,'application/pdf')
run('501-page PDF','pdf',pdf(501),20)
run('PDF open action','pdf',pdf(active=True),20)
run('damaged PDF','pdf',pdf()[:70],20)
secured=subprocess.run(['docker','run','--rm','-i','--entrypoint','/bin/sh','autolife-task003-pdf','-ec','cat >/tmp/input.pdf; qpdf --encrypt fixture fixture 256 -- /tmp/input.pdf /tmp/output.pdf; cat /tmp/output.pdf'],input=pdf(),capture_output=True,check=True,timeout=30)
run('password-protected PDF','pdf',secured.stdout,20)
