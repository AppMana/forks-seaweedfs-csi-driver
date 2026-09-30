import hashlib
import io
import json
from pathlib import Path
import tarfile
import tempfile
import unittest

from package_images import inspect_oci


def tar_bytes(files):
    out = io.BytesIO()
    with tarfile.open(fileobj=out, mode='w') as archive:
        for name, value in files.items():
            info = tarfile.TarInfo(name)
            info.size = len(value)
            archive.addfile(info, io.BytesIO(value))
    return out.getvalue()


class PackageImages(unittest.TestCase):
    def test_dll_only_target_preserves_stock_driver_installer(self):
        dockerfile = (Path(__file__).resolve().parents[2] /
                      'cmd/seaweedfs-mount/Dockerfile.Windows').read_text()
        self.assertIn('AS stock', dockerfile)
        self.assertIn('FROM stock AS dll-only', dockerfile)
        dll_stage = dockerfile.split('FROM stock AS dll-only', 1)[1]
        self.assertIn('COPY --from=verified-dll /out/winfsp-x64.dll /winfsp-x64.dll', dll_stage)
        self.assertNotIn('.sys', dll_stage)
        self.assertNotIn('msiexec', dll_stage)
        self.assertIn('ARG WINFSP_DLL_SHA256', dockerfile)
        self.assertIn('$WINFSP_DLL_SHA256  /out/winfsp-x64.dll', dockerfile)

    def test_oci_identity_and_corruption_controls(self):
        payload = b'qualified executable fixture'
        want = {'weed.exe': hashlib.sha256(payload).hexdigest()}
        for case in ('good', 'wrong-platform', 'wrong-entrypoint', 'wrong-executable',
                     'corrupt-blob', 'deleted-executable', 'opaque-directory',
                     'deleted-directory', 'opaque-root', 'opaque-and-replaced'):
            with self.subTest(case=case), tempfile.TemporaryDirectory() as directory:
                files = {}
                def blob(data):
                    digest = hashlib.sha256(data).hexdigest()
                    files['blobs/sha256/' + digest] = data
                    return {'digest': 'sha256:' + digest, 'size': len(data)}
                config = blob(json.dumps(dict(os='linux' if case == 'wrong-platform' else 'windows',
                    architecture='amd64', config={'Entrypoint': ['/wrong' if case == 'wrong-entrypoint' else '/weed.exe']})).encode())
                layer = blob(tar_bytes({'Files/weed.exe': b'other' if case == 'wrong-executable' else payload}))
                layers = [layer]
                if case == 'deleted-executable':
                    layers.append(blob(tar_bytes({'Files/.wh.weed.exe': b''})))
                if case == 'opaque-directory':
                    layers.append(blob(tar_bytes({'Files/.wh..wh..opq': b''})))
                if case == 'deleted-directory':
                    layers.append(blob(tar_bytes({'.wh.Files': b''})))
                if case == 'opaque-root':
                    layers.append(blob(tar_bytes({'.wh..wh..opq': b''})))
                if case == 'opaque-and-replaced':
                    layers.append(blob(tar_bytes({'Files/weed.exe': payload, 'Files/.wh..wh..opq': b''})))
                manifest = blob(json.dumps(dict(config=config, layers=layers)).encode())
                files['index.json'] = json.dumps({'manifests': [manifest]}).encode()
                if case == 'corrupt-blob':
                    files['blobs/sha256/' + layer['digest'][7:]] = b'corrupt'
                path = Path(directory) / 'image.tar'
                path.write_bytes(tar_bytes(files))
                if case in ('good', 'opaque-and-replaced'):
                    self.assertEqual(inspect_oci(path, 'windows', want, '/weed.exe')['files'], want)
                else:
                    with self.assertRaises(ValueError):
                        inspect_oci(path, 'windows', want, '/weed.exe')
