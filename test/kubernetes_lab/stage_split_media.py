#!/usr/bin/env python3
"""Compose pinned existing packages onto offline media; never rebuild or publish."""
import argparse
import hashlib
import json
from pathlib import Path
import subprocess
import tarfile
import zipfile


def sha(path):
    with path.open('rb') as stream:
        return hashlib.file_digest(stream, 'sha256').hexdigest()


def pinned(path, expected):
    if sha(path) != expected:
        raise ValueError(f'input digest mismatch: {path}')


def main():
    p = argparse.ArgumentParser(description=__doc__)
    for name in ('base', 'images_manifest', 'package_results', 'certificate', 'native'):
        p.add_argument('--' + name.replace('_', '-'), type=Path, required=True)
        p.add_argument('--' + name.replace('_', '-') + '-sha256', required=True)
    p.add_argument('--output', type=Path, required=True)
    args = p.parse_args()
    for name in ('base', 'images_manifest', 'package_results', 'certificate', 'native'):
        pinned(getattr(args, name), getattr(args, name + '_sha256'))
    args.output.mkdir(parents=True, exist_ok=False)
    with zipfile.ZipFile(args.package_results) as archive:
        package = json.loads(archive.read('package-manifest.json'))
        if not package['lab_only'] or package['production_qualified']:
            raise ValueError('expected lab-only MSI')
        msi = archive.read(f"winfsp-{package['version']}-LAB-ONLY.msi")
    if hashlib.sha256(msi).hexdigest() != package['package_sha256']:
        raise ValueError('MSI digest mismatch')
    (args.output / 'candidate.msi').write_bytes(msi)
    thumb = subprocess.check_output(['openssl', 'x509', '-inform', 'DER', '-in', str(args.certificate),
                                    '-noout', '-fingerprint', '-sha1'], text=True).strip().split('=')[1].replace(':', '')
    payload = package['payload']
    candidate = dict(lab_only=True, certificate_thumbprint=thumb,
                     driver_source_revision=payload['source_revision'],
                     driver_source_archive_sha256=payload['source_archive_sha256'],
                     driver_sha256=payload['files']['winfsp-x64.sys'],
                     dll_sha256=payload['files']['winfsp-x64.dll'])
    candidate_path = args.output / 'candidate.json'
    candidate_path.write_text(json.dumps(candidate, indent=2) + '\n')
    workload = ['-test.v', '-test.run=^TestCSICandidateWinFsp$', '-test.timeout=35m', '-csi-live',
                '-csi-cni=calico-vxlan', '-csi-candidate-manifest=/mnt/qualification/candidate.json',
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
