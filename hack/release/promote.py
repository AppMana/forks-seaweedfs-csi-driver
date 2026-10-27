#!/usr/bin/env python3
"""Promote a CI candidate's exact images to a readable release tag.

build-images.yml publishes every build as candidate-<sha>-<run>-<attempt>.
A release names the candidate's bytes, never new ones: for both images and
every platform the release tag is added to the candidate's manifest (crane
tag, no rebuild), and an existing release tag is never moved. The release
must have a RELEASES.md section that names its source commit. Prints the
immutable pins deployments use, repository:<release tag>@<digest>.
"""
import argparse
import os
import re
import subprocess
from pathlib import Path

REGISTRY = 'ghcr.io/appmana'
REPOSITORIES = ('seaweedfs-csi-driver', 'seaweedfs-mount')
# The multi-platform manifest list and the per-platform images it lists;
# Linux and Windows DaemonSets pin the per-platform digests.
SUFFIXES = ('', '-linux-amd64', '-windows-ltsc2022')
RELEASE_RE = re.compile(r'v\d+\.\d+\.\d+-appmana\.post\.\d+')
CANDIDATE_RE = re.compile(r'candidate-(?P<sha>[0-9a-f]{40})-\d+-\d+')


class ReleaseError(Exception):
    pass


def plan(release, candidate):
    if not RELEASE_RE.fullmatch(release):
        raise ReleaseError(f'release tag {release!r} is not v<upstream>-appmana.post.<n>')
    if not CANDIDATE_RE.fullmatch(candidate):
        raise ReleaseError(f'{candidate!r} is not a build-images.yml candidate tag')
    return [(f'{REGISTRY}/{repo}:{candidate}{suffix}', f'{REGISTRY}/{repo}:{release}{suffix}')
            for repo in REPOSITORIES for suffix in SUFFIXES]


def source_commit(candidate):
    match = CANDIDATE_RE.fullmatch(candidate)
    if not match:
        raise ReleaseError(f'{candidate!r} is not a build-images.yml candidate tag')
    return match.group('sha')


def release_notes_section(notes, release, sha):
    text = Path(notes).read_text()
    sections = re.split(r'(?m)^## ', text)
    for section in sections[1:]:
        heading, _, body = section.partition('\n')
        if heading.strip() == release:
            if sha not in body:
                raise ReleaseError(f'{notes} section {release} does not name source commit {sha}')
            return body
    raise ReleaseError(f'{notes} has no section "## {release}"')


def crane(args):
    result = subprocess.run(['crane', *args], capture_output=True, text=True, timeout=120)
    if result.returncode != 0:
        raise ReleaseError(f'crane {" ".join(args)}: {result.stderr.strip()}')
    return result.stdout.strip()


def digest_or_none(run, reference):
    try:
        return run(['digest', reference])
    except ReleaseError as error:
        if 'MANIFEST_UNKNOWN' in str(error) or 'NAME_UNKNOWN' in str(error):
            return None
        raise


def promote(release, candidate, run=crane):
    pairs = plan(release, candidate)
    # Resolve and check everything before the first tag is written.
    digests = {}
    for source, target in pairs:
        digest = digest_or_none(run, source)
        if digest is None:
            raise ReleaseError(f'candidate image {source} does not exist')
        existing = digest_or_none(run, target)
        if existing is not None and existing != digest:
            raise ReleaseError(f'{target} already names {existing}, not {digest}; release tags never move')
        digests[source] = digest
    pins = []
    for source, target in pairs:
        if digest_or_none(run, target) is None:
            run(['tag', source, target.rsplit(':', 1)[1]])
        promoted = run(['digest', target])
        if promoted != digests[source]:
            raise ReleaseError(f'{target} resolves to {promoted}, expected {digests[source]}')
        pins.append(f'{target}@{promoted}')
    return pins


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--release', required=True, help='v<upstream>-appmana.post.<n>')
    parser.add_argument('--candidate', required=True, help='candidate-<sha>-<run>-<attempt>')
    parser.add_argument('--notes', default='RELEASES.md')
    args = parser.parse_args()
    release_notes_section(args.notes, args.release, source_commit(args.candidate))
    pins = promote(args.release, args.candidate)
    lines = [f'Release {args.release} from {args.candidate}:', '', *[f'- `{pin}`' for pin in pins]]
    print('\n'.join(lines))
    summary = os.environ.get('GITHUB_STEP_SUMMARY')
    if summary:
        with open(summary, 'a') as stream:
            stream.write('\n'.join(lines) + '\n')


if __name__ == '__main__':
    try:
        main()
    except ReleaseError as error:
        raise SystemExit(f'error: {error}')
