#!/usr/bin/env python3
"""Publish verified OCI bytes under a digest-derived candidate tag, never a release tag."""
import argparse
import json
from pathlib import Path, PurePosixPath
import re
import subprocess
import tarfile
import tempfile

from package_images import inspect_oci, sha


def destination(component, digest):
    if component not in ('mount', 'csi-driver') or not re.fullmatch(r'sha256:[0-9a-f]{64}', digest):
        raise ValueError('expected CSI component and full manifest digest')
    return 'ghcr.io/appmana/seaweedfs-' + component + ':candidate-' + digest[7:]


def verify(archive, record, component):
    target = destination(component, record['manifest_sha256'])
    if record['status'] != 'passed' or sha(archive) != record['archive_sha256']:
        raise ValueError('archive does not match passing packaging record')
    entrypoint = '/seaweedfs-' + component + ('.exe' if record['platform'] == 'windows' else '')
    observed = inspect_oci(archive, record['platform'], record['files'], entrypoint)
    for key in ('manifest_sha256', 'config_sha256', 'files', 'entrypoint', 'platform'):
        if observed[key] != record[key]:
            raise ValueError('image verification mismatch: ' + key)
    return target


def extract_layout(archive, directory):
    # Only OCI metadata and digest blobs; reject links, duplicate paths and
    # traversal before extracting anything, even from locally built archives.
    with tarfile.open(archive) as source:
        seen = set()
        for member in source.getmembers():
            path = PurePosixPath(member.name)
            name = str(path)
            allowed = name in ('index.json', 'oci-layout', 'blobs', 'blobs/sha256') or bool(
                re.fullmatch(r'blobs/sha256/[0-9a-f]{64}', name))
            if path.is_absolute() or '..' in path.parts or not allowed or name in seen:
                raise ValueError('unsafe or duplicate OCI member')
            if not member.isfile() and not member.isdir():
                raise ValueError('OCI member must be a regular file or directory')
            seen.add(name)
        source.extractall(directory, filter='data')


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--archive', required=True, type=Path)
    parser.add_argument('--record', required=True, type=Path,
                        help='packaging result JSON; not a runtime qualification waiver')
    parser.add_argument('--component', required=True, choices=('mount', 'csi-driver'))
    parser.add_argument('--results-root', required=True, type=Path)
    parser.add_argument('--publish', action='store_true', help='explicitly upload; default is local verification only')
    args = parser.parse_args()
    record = json.loads(args.record.read_text())
    target = verify(args.archive, record, args.component)
    if not args.publish:
        print(json.dumps(dict(status='verified_not_published', destination=target)))
        return
    args.results_root.mkdir(parents=True, exist_ok=True)
    output = Path(tempfile.mkdtemp(prefix='candidate-publication-', dir=args.results_root))
    layout = output / 'oci'
    layout.mkdir()
    extract_layout(args.archive, layout)
    # Preserve retained OCI bytes/evidence even if the registry is unavailable.
    subprocess.run(['crane', 'push', str(layout), target], check=True, timeout=600)
    digest = subprocess.check_output(['crane', 'digest', target], text=True, timeout=60).strip()
    if digest != record['manifest_sha256']:
        raise ValueError('published digest differs from verified image')
    receipt = dict(status='published_candidate', image=target, digest=digest,
                   archive_sha256=record['archive_sha256'],
                   scope='exact packaging bytes; not release approval or production deployment')
    (output / 'receipt.json').write_text(json.dumps(receipt, indent=2) + '\n')
    print(json.dumps(receipt))


if __name__ == '__main__':
    main()
