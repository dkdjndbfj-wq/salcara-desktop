import concurrent.futures
import hashlib
import json
from pathlib import Path, PurePosixPath
import time
import urllib.request
import zipfile

expected = json.loads(Path('.release-verification/expected.json').read_text())
root = Path('verified-public-assets')
root.mkdir()
report = {'releases': [], 'assets': []}

def read_url(url, accept='application/vnd.github+json'):
 request = urllib.request.Request(url, headers={'User-Agent':'Salcara-public-release-verification','Accept':accept})
 return urllib.request.urlopen(request, timeout=60)

def digest_file(path):
 with path.open('rb') as stream:
  return hashlib.file_digest(stream, 'sha256').hexdigest()

jobs = []
for product, item in expected.items():
 repo, tag = item['repository'], item['tag']
 with read_url(f'https://api.github.com/repos/{repo}/releases/tags/{tag}') as response:
  release = json.load(response)
 with read_url(f'https://api.github.com/repos/{repo}/git/ref/tags/{tag}') as response:
  reference = json.load(response)
 assert reference['object']['type'] == 'commit' and reference['object']['sha'] == item['sourceSHA']
 assert release['draft'] is False and release['prerelease'] is False and release['published_at']
 actual = {a['name']: {'digest':a['digest'],'bytes':a['size']} for a in release['assets']}
 assert actual == item['assets'], 'Public asset inventory differs from verified draft'
 directory = root / product
 directory.mkdir()
 for asset in release['assets']:
  assert '/' not in asset['name'] and '\\' not in asset['name'] and ':' not in asset['name']
  jobs.append((product, directory, asset))
 report['releases'].append({'product':product,'tag':tag,'sourceSHA':item['sourceSHA'],'url':release['html_url'],'publishedAt':release['published_at']})

def download(job):
 product, directory, asset = job
 path = directory / asset['name']
 for attempt in range(3):
  try:
   with read_url(asset['browser_download_url'], 'application/octet-stream') as response, path.open('wb') as stream:
    assert response.status == 200
    while True:
     chunk = response.read(1024 * 1024)
     if not chunk:
      break
     stream.write(chunk)
   assert path.stat().st_size == asset['size']
   assert 'sha256:' + digest_file(path) == asset['digest']
   result = {'product':product,'name':asset['name'],'bytes':asset['size'],'digest':asset['digest'],'anonymousPublicDownload':True}
   print(json.dumps(result), flush=True)
   return result
  except Exception:
   if attempt == 2:
    raise
   time.sleep(attempt + 1)
with concurrent.futures.ThreadPoolExecutor(max_workers=6) as pool:
 report['assets'] = list(pool.map(download, jobs))

mobile = root / 'mobile' / 'salcara-image-android-v1.8.3.apk'
with zipfile.ZipFile(mobile) as archive:
 assert archive.testzip() is None
 libraries = [name for name in archive.namelist() if name.startswith('lib/') and name.endswith('.so')]
 assert len(libraries) == 52
 assert {name.split('/')[1] for name in libraries} == {'arm64-v8a','armeabi-v7a'}
 for name in libraries:
  header = archive.read(name)[:20]
  assert header[:4] == b'\x7fELF'
  assert header[4] == (2 if name.split('/')[1] == 'arm64-v8a' else 1)
  assert int.from_bytes(header[18:20],'little') == (183 if name.split('/')[1] == 'arm64-v8a' else 40)
report['androidZIPAnd52NativeLibrariesVerified'] = True
portable = root / 'desktop' / 'Salcara-Bridge-1.6.5-win32-x64.zip'
with zipfile.ZipFile(portable) as archive:
 assert archive.testzip() is None
 for entry in archive.infolist():
  path = PurePosixPath(entry.filename)
  assert not path.is_absolute() and '..' not in path.parts and '\\' not in entry.filename and ':' not in entry.filename
 archive.extractall('verified-portable')

package = 'dkdjndbfj-wq/salcara-hub-standalone'
with read_url('https://ghcr.io/token?service=ghcr.io&scope=repository%3A'+package+'%3Apull') as response:
 token = json.load(response)['token']
request = urllib.request.Request('https://ghcr.io/v2/'+package+'/manifests/0.5.3', headers={'Authorization':'Bearer '+token,'Accept':'application/vnd.oci.image.index.v1+json'})
with urllib.request.urlopen(request,timeout=30) as response:
 index = response.read()
assert 'sha256:'+hashlib.sha256(index).hexdigest() == 'sha256:b930e8b7d5545be57c694952f54e15e7eec1f3bb61103b02cacf2dea9dfe91f7'
report['publicGHCRIndexVerified'] = True
report['all18PublicAssetsVerified'] = len(report['assets']) == 18
(root / 'verification.json').write_text(json.dumps(report,indent=2)+'\n')
print(json.dumps({'all18PublicAssetsVerified':True,'androidZIPAnd52NativeLibrariesVerified':True,'publicGHCRIndexVerified':True}),flush=True)
