#!/usr/bin/env python3
"""Package and verify exact qualified CSI bytes. No rebuild, push or VM start."""
import argparse
import concurrent.futures
import hashlib
import io
import json
from pathlib import Path
import re
import subprocess
import tarfile
import tempfile
import time
import uuid


def sha(path):
    with path.open('rb') as stream:
        return hashlib.file_digest(stream, 'sha256').hexdigest()


def smoke_linux(image_id, component, output):
    """Start the packaged entrypoint with private tmpfs, no network or mounts."""
    name = 'seaweedfs-csi-image-check-' + uuid.uuid4().hex
    def docker(*args, check=True):
        return subprocess.run(['docker', *args], check=check, text=True,
                              stdout=subprocess.PIPE, stderr=subprocess.STDOUT, timeout=30)
    created = False
    try:
        args = ['--endpoint=unix:///tmp/qualification.sock']
        if component == 'csi-driver':
            args += ['--components=controller', '--filer=127.0.0.1:8888']
        docker('create', '--pull=never', '--name', name, '--network=none',
               '--read-only', '--user=1000:1000', '--cap-drop=ALL',
               '--security-opt=no-new-privileges', '--memory=512m', '--memory-swap=512m',
               '--cpus=1', '--pids-limit=128', '--tmpfs=/tmp:rw,mode=1777,size=32m',
               '--log-opt=max-size=8m', '--log-opt=max-file=1', image_id, *args)
        created = True
        docker('start', name)
        deadline = time.monotonic() + 20
        while docker('exec', name, 'test', '-S', '/tmp/qualification.sock', check=False).returncode:
            if time.monotonic() >= deadline:
                raise RuntimeError('packaged service did not create its listening socket')
            time.sleep(0.5)
        if component == 'mount':
            (output / 'mount-linux-weed-version.txt').write_text(
                docker('exec', name, '/usr/bin/weed', 'version').stdout)
    finally:
        if created:
            try:
                (output / (component + '-linux-startup.log')).write_text(docker('logs', name).stdout)
                docker('stop', '--time=5', name)
                (output / (component + '-linux-container.json')).write_text(docker('inspect', name).stdout)
            finally:
                docker('rm', '--force', '--volumes', name)


def inspect_oci(path, platform, expected, entrypoint):
    """Inspect layer bytes in order without extracting archive paths to disk."""
    with tarfile.open(path) as archive:
        def blob(digest):
            if not re.fullmatch(r'sha256:[0-9a-f]{64}', digest):
                raise ValueError('invalid OCI digest')
            data = archive.extractfile('blobs/sha256/' + digest[7:]).read()
            if hashlib.sha256(data).hexdigest() != digest[7:]:
                raise ValueError('OCI blob digest mismatch')
            return data
        index = json.load(archive.extractfile('index.json'))
        if len(index['manifests']) != 1:
            raise ValueError('expected one platform manifest')
        manifest = json.loads(blob(index['manifests'][0]['digest']))
        config = json.loads(blob(manifest['config']['digest']))
        if config['os'] != platform or config['architecture'] != 'amd64':
            raise ValueError('image platform mismatch')
        if config['config']['Entrypoint'] != [entrypoint]:
            raise ValueError('image entrypoint mismatch')
        observed = {}
        prefix = 'Files/' if platform == 'windows' else ''
        for layer in manifest['layers']:
            with tarfile.open(fileobj=io.BytesIO(blob(layer['digest'])), mode='r:*') as contents:
                members = contents.getmembers()
                # OCI whiteouts affect prior layers, not siblings written in
                # this layer. Directory and root opacity must remove descendants.
                for item in members:
                    name = item.name.removeprefix('./').lstrip('/')
                    parent, _, leaf = name.rpartition('/')
                    if not leaf.startswith('.wh.'):
                        continue
                    removed = parent if leaf == '.wh..wh..opq' else (parent + '/' if parent else '') + leaf[4:]
                    for target in list(observed):
                        full = prefix + target
                        if not removed or full == removed or full.startswith(removed + '/'):
                            del observed[target]
                for item in members:
                    name = item.name.removeprefix('./').lstrip('/')
                    for target in expected:
                        full = prefix + target
                        if name == full:
                            if not item.isfile():
                                raise ValueError('executable is not a regular file: ' + full)
                            observed[target] = hashlib.file_digest(contents.extractfile(item), 'sha256').hexdigest()
        if observed != expected:
            raise ValueError(f'packaged executable digest mismatch: {observed} != {expected}')
        return dict(manifest_sha256=index['manifests'][0]['digest'],
                    config_sha256=manifest['config']['digest'], files=observed,
                    entrypoint=config['config']['Entrypoint'], platform=platform)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--inputs', type=Path, required=True)
    parser.add_argument('--sha256-file', type=Path, required=True)
    parser.add_argument('--results-root', type=Path, required=True)
    parser.add_argument('--builder', required=True)
    parser.add_argument('--winfsp-dll', action='store_true',
                        help='package pinned app-local winfsp-x64.dll; retain the stock MSI/driver')
    args = parser.parse_args()
    pins = {}
    for line in args.sha256_file.read_text().splitlines():
        digest, name = line.split(maxsplit=1)
        if not re.fullmatch(r'[0-9a-f]{64}', digest) or not re.fullmatch(r'[A-Za-z0-9_.-]+', name) or name in pins:
            parser.error('invalid or duplicate pinned filename')
        pins[name] = digest
    required = ['weed', 'weed.exe', 'seaweedfs-mount', 'seaweedfs-mount.exe',
                'seaweedfs-csi-driver', 'seaweedfs-csi-driver.exe', 'winfsp.msi']
    if args.winfsp_dll:
        required.append('winfsp-x64.dll')
        if pins.get('winfsp.msi') != '073a70e00f77423e34bed98b86e600def93393ba5822204fac57a29324db9f7a':
            parser.error('DLL-only packaging requires the pinned official WinFsp MSI')
    for name in required:
        if name not in pins or sha(args.inputs / name) != pins[name]:
            parser.error('missing or mismatched input: ' + name)
    args.results_root.mkdir(parents=True, exist_ok=True)
    output = Path(tempfile.mkdtemp(prefix='production-images.', dir=args.results_root))
    print(output, flush=True)
    repo = Path(__file__).resolve().parents[2]

    def build(platform, component):
        windows = platform == 'windows'
        suffix = '.exe' if windows else ''
        binary = 'seaweedfs-' + component + suffix
        dockerfile = repo / 'cmd' / ('seaweedfs-' + component) / ('Dockerfile.Windows' if windows else 'Dockerfile')
        name = component + '-' + platform
        archive = output / (name + '.tar')
        expected = {binary: pins[binary]}
        build_args = {'PAYLOAD_STAGE': 'prebuilt',
                      ('MOUNT_SHA256' if component == 'mount' else 'DRIVER_SHA256'): pins[binary]}
        if component == 'mount':
            expected['weed.exe' if windows else 'usr/bin/weed'] = pins['weed' + suffix]
            build_args['WEED_SHA256'] = pins['weed' + suffix]
            if windows:
                expected['winfsp.msi'] = pins['winfsp.msi']
                build_args['WINFSP_MSI_SHA256'] = pins['winfsp.msi']
        command = ['docker', 'buildx', 'build', '--builder', args.builder,
                   '--platform', platform + '/amd64', '--provenance=false', '--sbom=false',
                   '--metadata-file', str(output / (name + '-build.json')),
                   '--output', 'type=oci,dest=' + str(archive),
                   '-t', 'appmana/seaweedfs-' + component + ':' + output.name.lower() + '-' + platform,
                   '-f', str(dockerfile)]
        if args.winfsp_dll and windows and component == 'mount':
            expected['winfsp-x64.dll'] = pins['winfsp-x64.dll']
            build_args['WINFSP_DLL_SHA256'] = pins['winfsp-x64.dll']
            command += ['--target', 'dll-only']
        for key, value in build_args.items():
            command += ['--build-arg', key + '=' + value]
        command += [str(args.inputs.resolve())]
        result = dict(status='failed', dockerfile_sha256=sha(dockerfile), command=command)
        try:
            with (output / (name + '.log')).open('w') as log:
                subprocess.run(command, stdout=log, stderr=subprocess.STDOUT, check=True, timeout=600)
            result.update(inspect_oci(archive, platform, expected, '/' + binary))
            if platform == 'linux':
                # Re-export cached layers into the local daemon. Classic Docker
                # cannot load OCI archives; verify the Docker export is identical.
                load = command.copy()
                load[load.index('--output') + 1] = 'type=docker'
                load[load.index('--metadata-file') + 1] = str(output / (name + '-docker-build.json'))
                with (output / (name + '-load.log')).open('w') as log:
                    subprocess.run(load, stdout=log, stderr=subprocess.STDOUT, check=True, timeout=120)
                image_tag = command[command.index('-t') + 1]
                loaded = subprocess.check_output(['docker', 'image', 'inspect', '--format', '{{.Id}}', image_tag], text=True, timeout=30).strip()
                if loaded != result['config_sha256']:
                    raise ValueError('Docker export differs from inspected OCI image')
                smoke_linux(loaded, component, output)
                result['startup'] = 'passed: isolated real entrypoint and listening socket'
            result.update(status='passed', archive_sha256=sha(archive))
        except Exception as exc:
            result['error'] = str(exc)
        (output / (name + '-result.json')).write_text(json.dumps(result, indent=2) + '\n')
        print(name + ': ' + result['status'], flush=True)
        return name, result

    with concurrent.futures.ThreadPoolExecutor(max_workers=4) as pool:
        pending = [pool.submit(build, platform, component)
                   for platform in ('linux', 'windows') for component in ('csi-driver', 'mount')]
        results = dict(job.result() for job in pending)
    passed = all(result['status'] == 'passed' for result in results.values())
    (output / 'manifest.json').write_text(json.dumps(dict(
        status='passed' if passed else 'failed', scope='packaging identity and Linux startup; not full CSI/Windows runtime qualification',
        input_manifest_sha256=sha(args.sha256_file), images=results), indent=2) + '\n')
    return 0 if passed else 1


if __name__ == '__main__':
    raise SystemExit(main())
