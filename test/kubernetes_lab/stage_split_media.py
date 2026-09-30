#!/usr/bin/env python3
"""Compose pinned existing packages onto offline media; never rebuild or publish."""
import argparse
import hashlib
import json
import os
from pathlib import Path
import re
import subprocess
import tarfile
import zipfile


def sha(path):
    with path.open('rb') as stream:
        return hashlib.file_digest(stream, 'sha256').hexdigest()


def pinned(path, expected):
    if sha(path) != expected:
        raise ValueError(f'input digest mismatch: {path}')


def certificate_identity(path):
    subject = subprocess.check_output([
        'openssl', 'x509', '-inform', 'DER', '-in', str(path),
        '-noout', '-subject', '-nameopt', 'RFC2253'], text=True).strip()
    if subject != 'subject=CN=AppMana WinFsp MSI LAB ONLY':
        raise ValueError(f'expected MSI lab certificate, got {subject}')
    thumb = subprocess.check_output([
        'openssl', 'x509', '-inform', 'DER', '-in', str(path),
        '-noout', '-fingerprint', '-sha1'], text=True).strip().split('=')[1].replace(':', '')
    return thumb


def launch_spec(bundle, cni):
    """Validate a composed bundle and select its existing candidate consumer.

    This is argument validation, not qualification evidence. The native fixture
    verifies the actual ISO digest before starting VMs; guest oracles verify the
    contained MSI, native executable, driver and runtime image identities.
    """
    if cni not in ('calico-vxlan', 'calico-bgp'):
        raise ValueError('unsupported CSI CNI')
    inputs = json.loads((bundle / 'inputs.json').read_text())
    if (Path(os.environ['LABCONTAINERS_CALICO_MEDIA']).resolve() !=
            (bundle / 'qualification.iso').resolve() or
            os.environ['LABCONTAINERS_CALICO_MEDIA_SHA256'] != inputs['iso_sha256'] or
            not re.fullmatch('[0-9a-f]{64}', inputs['iso_sha256'])):
        raise ValueError('candidate bundle does not match the selected offline media')
    args = json.loads((bundle / 'workload-args.json').read_text())
    if not isinstance(args, list) or not all(isinstance(a, str) for a in args):
        raise ValueError('expected a string argument array')
    values = {}
    for arg in args:
        key, _, value = arg.partition('=')
        if key in values:
            raise ValueError('duplicate workload argument: '+key)
        values[key] = value
    if values.get('-csi-cni') not in ('calico-vxlan', 'calico-bgp'):
        raise ValueError('invalid bundled CSI CNI')
    expected = {
        '-test.v': '', '-test.run': '^TestCSICandidateWinFsp$', '-test.timeout': '35m',
        '-csi-live': '', '-csi-cni': values['-csi-cni'],
        '-csi-candidate-manifest': '/mnt/qualification/candidate.json',
        '-csi-candidate-manifest-sha256': sha(bundle / 'candidate.json'),
        '-csi-candidate-msi-sha256': inputs['package_sha256'],
        '-csi-native-test-executable': r'C:\tools\winfsp-csi-candidate.test.exe',
        '-csi-candidate-native-test-sha256': inputs['inputs']['native_sha256'],
    }
    pinned(bundle / 'candidate.msi', inputs['package_sha256'])
    if not re.fullmatch('[0-9a-f]{64}', inputs['inputs']['native_sha256']):
        raise ValueError('invalid native executable digest')
    for role in ('driver', 'mount'):
        for platform in ('linux', 'windows'):
            key = f'-csi-{role}-{platform}-image'
            value = values.get(key, '')
            if not re.fullmatch(r'[a-zA-Z0-9./:_-]+@sha256:[0-9a-f]{64}', value):
                raise ValueError('missing or unpinned final image: '+key)
            expected[key] = value
    if values != expected:
        raise ValueError('candidate workload arguments differ from the composed bundle contract')
    return {'args': ['-csi-cni='+cni if a.startswith('-csi-cni=') else a for a in args],
            'success': 'CSI_CANDIDATE_DRIVER_QUALIFICATION_COMPLETE'}


def main():
    p = argparse.ArgumentParser(description=__doc__)
    p.add_argument('--launch-bundle', type=Path)
    p.add_argument('--cni', choices=('calico-vxlan', 'calico-bgp'), default='calico-vxlan')
    for name in ('base', 'images_manifest', 'package_results', 'certificate', 'native'):
        p.add_argument('--' + name.replace('_', '-'), type=Path)
        p.add_argument('--' + name.replace('_', '-') + '-sha256')
    p.add_argument('--output', type=Path)
    args = p.parse_args()
    if args.launch_bundle:
        print(json.dumps(launch_spec(args.launch_bundle, args.cni)))
        return
    if args.output is None or any(getattr(args, name) is None or getattr(args, name+'_sha256') is None
                                 for name in ('base', 'images_manifest', 'package_results', 'certificate', 'native')):
        p.error('composition requires --output and all five explicit input paths and SHA-256 pins')
    for name in ('base', 'images_manifest', 'package_results', 'certificate', 'native'):
        pinned(getattr(args, name), getattr(args, name + '_sha256'))
    thumb = certificate_identity(args.certificate)
    args.output.mkdir(parents=True, exist_ok=False)
    with zipfile.ZipFile(args.package_results) as archive:
        package = json.loads(archive.read('package-manifest.json'))
        if not package['lab_only'] or package['production_qualified']:
            raise ValueError('expected lab-only MSI')
        msi = archive.read(f"winfsp-{package['version']}-LAB-ONLY.msi")
    if hashlib.sha256(msi).hexdigest() != package['package_sha256']:
        raise ValueError('MSI digest mismatch')
    (args.output / 'candidate.msi').write_bytes(msi)
    payload = package['payload']
    candidate = dict(lab_only=True, certificate_thumbprint=thumb,
                     driver_source_revision=payload['source_revision'],
                     driver_source_archive_sha256=payload['source_archive_sha256'],
                     driver_sha256=payload['files']['winfsp-x64.sys'],
                     dll_sha256=payload['files']['winfsp-x64.dll'])
    candidate_path = args.output / 'candidate.json'
    candidate_path.write_text(json.dumps(candidate, indent=2) + '\n')
    workload = ['-test.v', '-test.run=^TestCSICandidateWinFsp$', '-test.timeout=35m', '-csi-live',
                '-csi-cni=' + args.cni, '-csi-candidate-manifest=/mnt/qualification/candidate.json',
                '-csi-candidate-manifest-sha256=' + sha(candidate_path),
                '-csi-candidate-msi-sha256=' + package['package_sha256'],
                r'-csi-native-test-executable=C:\tools\winfsp-csi-candidate.test.exe',
                '-csi-candidate-native-test-sha256=' + args.native_sha256]
    maps = [(args.output / 'candidate.msi', '/candidate.msi'), (args.certificate, '/candidate.cer'),
            (args.native, '/winfsp-csi-candidate.test.exe'), (candidate_path, '/candidate.json')]
    images = json.loads(args.images_manifest.read_text())
    if images['status'] != 'passed':
        raise ValueError('image packaging checks did not pass')
    for platform in ('linux', 'windows'):
        for component, role in (('csi-driver', 'driver'), ('mount', 'mount')):
            key = component + '-' + platform
            entry = images['images'][key]
            path = args.images_manifest.parent / (key + '.tar')
            pinned(path, entry['archive_sha256'])
            with tarfile.open(path) as archive:
                index = json.load(archive.extractfile('index.json'))
            descriptor, = index['manifests']
            if descriptor['digest'] != entry['manifest_sha256']:
                raise ValueError('OCI root digest mismatch')
            name = descriptor['annotations']['io.containerd.image.name']
            workload.append(f'-csi-{role}-{platform}-image={name}@{descriptor["digest"]}')
            maps.append((path, f'/{platform}-final-{role}.tar'))
    cmd = ['xorriso', '-indev', str(args.base), '-outdev', str(args.output / 'qualification.iso'),
           '-joliet', 'on', '-volid', 'LCQUAL']
    for source, destination in maps:
        cmd.extend(['-map', str(source), destination])
    subprocess.run(cmd + ['-commit', '-end'], check=True)
    subprocess.run(['bash', str(Path(__file__).with_name('verify-media.sh')),
                    str(args.output / 'qualification.iso')], check=True)
    visible = subprocess.check_output(['isoinfo', '-J', '-f', '-i',
                                       str(args.output / 'qualification.iso')], text=True).splitlines()
    for _, destination in maps:
        if destination not in visible:
            raise ValueError(f'Windows-visible added input missing: {destination}')
    (args.output / 'workload-args.json').write_text(json.dumps(workload) + '\n')
    (args.output / 'inputs.json').write_text(json.dumps({
        'inputs': {k: str(v) for k, v in vars(args).items()},
        'iso_sha256': sha(args.output / 'qualification.iso'),
        'package_sha256': package['package_sha256'], 'candidate': candidate,
        'qualification': 'not run'}, indent=2) + '\n')


if __name__ == '__main__':
    main()
