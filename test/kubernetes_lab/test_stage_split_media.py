import hashlib
import io
import json
from pathlib import Path
import shutil
import subprocess
import tarfile
import tempfile
import unittest
from unittest.mock import patch
import zipfile

import stage_split_media as stage


class SplitMediaTest(unittest.TestCase):
    def test_msi_rejects_manual_driver_certificate_before_composition(self):
        with patch.object(stage.subprocess, 'check_output', return_value='subject=CN=AppMana WinFsp LAB ONLY\n'):
            with self.assertRaisesRegex(ValueError, 'expected MSI lab certificate'):
                stage.certificate_identity(Path('unused.der'))

    def test_pin_rejects_changed_input(self):
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / 'input'
            path.write_bytes(b'original')
            digest = stage.sha(path)
            stage.pinned(path, digest)
            path.write_bytes(b'changed')
            with self.assertRaisesRegex(ValueError, 'digest mismatch'):
                stage.pinned(path, digest)

    def test_real_iso_composition_and_argument_pins(self):
        if any(shutil.which(tool) is None for tool in ('xorriso', 'isoinfo', 'openssl')):
            self.skipTest('ISO and certificate tools required')
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            inputs = root / 'base-inputs'
            inputs.mkdir()
            names = ('k0s k0s.exe mount-smoke.ps1 linux-csi.tar linux-registrar.tar '
                     'linux-provisioner.tar linux-attacher.tar linux-resizer.tar windows-csi.tar '
                     'windows-registrar.tar windows-pause.tar windows-workload.tar').split()
            for name in names:
                (inputs / name).write_bytes(b'fixture')
            base = root / 'base.iso'
            subprocess.run(['xorriso', '-as', 'mkisofs', '-R', '-J', '-V', 'LCQUAL', '-o',
                            str(base), str(inputs)], check=True, capture_output=True)
            cert = root / 'cert.der'
            subprocess.run(['openssl', 'req', '-x509', '-newkey', 'rsa:2048', '-nodes',
                            '-subj', '/CN=AppMana WinFsp MSI LAB ONLY', '-days', '1',
                            '-keyout', str(root / 'key.pem'), '-outform', 'DER', '-out', str(cert)],
                           check=True, capture_output=True)
            native = root / 'native.exe'
            native.write_bytes(b'native-fixture')
            package = root / 'results.zip'
            msi = b'msi-fixture'
            manifest = dict(version='1.0.0', lab_only=True, production_qualified=False,
                            package_sha256=hashlib.sha256(msi).hexdigest(),
                            payload=dict(source_revision='a'*40, source_archive_sha256='b'*64,
                                         files={'winfsp-x64.sys':'c'*64, 'winfsp-x64.dll':'d'*64}))
            with zipfile.ZipFile(package, 'w') as z:
                z.writestr('package-manifest.json', json.dumps(manifest))
                z.writestr('winfsp-1.0.0-LAB-ONLY.msi', msi)
            images = {'status':'passed', 'images':{}}
            for platform in ('linux', 'windows'):
                for component in ('csi-driver', 'mount'):
                    key = component + '-' + platform
                    path = root / (key + '.tar')
                    index = json.dumps({'manifests':[{'digest':'sha256:'+'e'*64,
                        'annotations':{'io.containerd.image.name':'example.test/'+key+':fixture'}}]}).encode()
                    with tarfile.open(path, 'w') as tar:
                        member = tarfile.TarInfo('index.json')
                        member.size = len(index)
                        tar.addfile(member, io.BytesIO(index))
                    images['images'][key] = {'archive_sha256':stage.sha(path), 'manifest_sha256':'sha256:'+'e'*64}
            images_path = root / 'images.json'
            images_path.write_text(json.dumps(images))
            args = ['python3', str(Path(stage.__file__))]
            for name, path in [('base',base), ('images-manifest',images_path), ('package-results',package), ('certificate',cert), ('native',native)]:
                args += ['--'+name, str(path), '--'+name+'-sha256', stage.sha(path)]
            output = root / 'output'
            result = subprocess.run(args+['--output',str(output)], capture_output=True, text=True)
            self.assertEqual(result.returncode, 0, result.stdout+result.stderr)
            workload = json.loads((output / 'workload-args.json').read_text())
            self.assertIn('-csi-candidate-msi-sha256='+manifest['package_sha256'], workload)
            self.assertEqual(sum('-image=' in arg for arg in workload), 4)
            evidence = json.loads((output / 'inputs.json').read_text())
            self.assertEqual(evidence['iso_sha256'], stage.sha(output / 'qualification.iso'))
            self.assertEqual(evidence['qualification'], 'not run')
            self.assertEqual((output / 'candidate.msi').read_bytes(), msi)
            # Refuse overwriting earlier evidence, including a partial run.
            repeat = subprocess.run(args+['--output',str(output)], capture_output=True)
            self.assertNotEqual(repeat.returncode, 0)


if __name__ == '__main__':
    unittest.main()
