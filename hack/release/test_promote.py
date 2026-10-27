import tempfile
import unittest
from pathlib import Path

from promote import ReleaseError, plan, promote, release_notes_section

SHA = '589aaadabbfa7703d23c8a39cdfaf53ac8a6750b'
CANDIDATE = 'candidate-' + SHA + '-40541552404-1'
RELEASE = 'v1.4.34-appmana.post.1'
NOTES = f"""# Releases

## v1.4.34-appmana.post.2

Source `{'a' * 40}`.

## {RELEASE}

Source `{SHA}`, CI run 40541552404.

- mount service drops optional flags its weed lacks
"""


def digest(n):
    return 'sha256:' + format(n, '064x')


class FakeRegistry:
    """crane digest/tag against an in-memory registry."""

    def __init__(self, tags):
        self.tags = dict(tags)
        self.calls = []

    def __call__(self, args):
        self.calls.append(args)
        if args[0] == 'digest':
            if args[1] not in self.tags:
                raise ReleaseError('MANIFEST_UNKNOWN ' + args[1])
            return self.tags[args[1]]
        if args[0] == 'tag':
            source, tag = args[1], args[2]
            repo = source.rsplit(':', 1)[0]
            self.tags[repo + ':' + tag] = self.tags[source]
            return ''
        raise AssertionError(args)


def candidate_registry():
    tags = {}
    n = 1
    for repo in ('seaweedfs-csi-driver', 'seaweedfs-mount'):
        for suffix in ('', '-linux-amd64', '-windows-ltsc2022'):
            tags[f'ghcr.io/appmana/{repo}:{CANDIDATE}{suffix}'] = digest(n)
            n += 1
    return tags


class Plan(unittest.TestCase):
    def test_release_tags_follow_the_fork_scheme(self):
        for bad in ('v1.4.34', 'v1.4.34-appmana.post', 'candidate-x', 'v1.4.34-appmana.post.1-linux-amd64', 'latest'):
            with self.subTest(bad=bad), self.assertRaises(ReleaseError):
                plan(bad, CANDIDATE)

    def test_only_ci_candidates_are_promoted(self):
        with self.assertRaises(ReleaseError):
            plan(RELEASE, 'candidate-589aaad-1-1')

    def test_every_image_and_platform_is_promoted(self):
        pairs = plan(RELEASE, CANDIDATE)
        self.assertEqual(len(pairs), 6)
        self.assertIn((f'ghcr.io/appmana/seaweedfs-mount:{CANDIDATE}-windows-ltsc2022',
                       f'ghcr.io/appmana/seaweedfs-mount:{RELEASE}-windows-ltsc2022'), pairs)


class Notes(unittest.TestCase):
    def test_section_must_exist_and_name_the_source_commit(self):
        with tempfile.TemporaryDirectory() as root:
            notes = Path(root) / 'RELEASES.md'
            notes.write_text(NOTES)
            self.assertIn('drops optional flags', release_notes_section(notes, RELEASE, SHA))
            with self.assertRaises(ReleaseError):
                release_notes_section(notes, 'v1.4.34-appmana.post.3', SHA)
            with self.assertRaises(ReleaseError):
                release_notes_section(notes, 'v1.4.34-appmana.post.2', SHA)


class Promote(unittest.TestCase):
    def test_promotes_the_exact_candidate_digests(self):
        registry = FakeRegistry(candidate_registry())
        pins = promote(RELEASE, CANDIDATE, registry)
        self.assertEqual(len(pins), 6)
        for pin in pins:
            reference, pinned = pin.split('@')
            self.assertEqual(registry.tags[reference], pinned)
        self.assertIn(f'ghcr.io/appmana/seaweedfs-mount:{RELEASE}-linux-amd64@{digest(5)}', pins)
        self.assertFalse([c for c in registry.calls if c[0] not in ('digest', 'tag')])

    def test_never_moves_an_existing_release_tag(self):
        tags = candidate_registry()
        tags[f'ghcr.io/appmana/seaweedfs-mount:{RELEASE}-linux-amd64'] = digest(99)
        registry = FakeRegistry(tags)
        with self.assertRaises(ReleaseError):
            promote(RELEASE, CANDIDATE, registry)
        self.assertFalse([c for c in registry.calls if c[0] == 'tag'])

    def test_rerun_of_a_completed_promotion_is_accepted(self):
        registry = FakeRegistry(candidate_registry())
        first = promote(RELEASE, CANDIDATE, registry)
        self.assertEqual(promote(RELEASE, CANDIDATE, registry), first)

    def test_missing_candidate_image_fails_before_tagging(self):
        tags = candidate_registry()
        del tags[f'ghcr.io/appmana/seaweedfs-csi-driver:{CANDIDATE}-windows-ltsc2022']
        registry = FakeRegistry(tags)
        with self.assertRaises(ReleaseError):
            promote(RELEASE, CANDIDATE, registry)
        self.assertFalse([c for c in registry.calls if c[0] == 'tag'])


if __name__ == '__main__':
    unittest.main()
