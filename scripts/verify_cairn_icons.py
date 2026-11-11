#!/usr/bin/env python3
"""检查 Cairn 图标的来源一致性；只读，不修改生产素材。"""
from pathlib import Path
from PIL import Image
import hashlib,json,sys
root=Path(__file__).resolve().parents[1]
manifest=json.loads((root/'studio/branding/icon-manifest.json').read_text(encoding='utf-8'))
master=root/'studio/branding/cairn_icon_master.png'
assert hashlib.sha256(master.read_bytes()).hexdigest()==manifest['master_sha256']
src=Image.open(master).convert('RGB')
for size,entry in manifest['sizes'].items():
 p=root/entry['path']; im=Image.open(p).convert('RGB'); n=int(size)
 assert im.size==(n,n),(p, im.size)
 assert hashlib.sha256(p.read_bytes()).hexdigest()==entry['sha256'],p
 # Every output must be a direct resize of the same master image, with no changes to the scene.
 assert list(im.getdata())==list(src.resize((n,n),Image.Resampling.LANCZOS).getdata()),p
 print('OK',n,p)
print('All PNG exports match one frozen source.')
