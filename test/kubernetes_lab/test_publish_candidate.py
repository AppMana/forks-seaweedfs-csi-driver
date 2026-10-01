import unittest
import io
from pathlib import Path
import tarfile
import tempfile
from unittest import mock
from publish_candidate import destination, verify, extract_layout


class CandidateDestination(unittest.TestCase):
    def test_mismatched_archive_rejected_before_image_inspection(self):
        with mock.patch('publish_candidate.sha', return_value='b' * 64), mock.patch(
                'publish_candidate.inspect_oci') as inspect:
            with self.assertRaises(ValueError):
                verify(Path('unused'), dict(status='passed', archive_sha256='a' * 64,
                       manifest_sha256='sha256:' + 'c' * 64), 'mount')
            inspect.assert_not_called()

    def test_extraction_rejects_links_traversal_duplicates_before_writes(self):
        for case in ('link', 'traversal', 'duplicate'):
            with self.subTest(case=case), tempfile.TemporaryDirectory() as root:
                archive = Path(root) / 'oci.tar'
                output = Path(root) / 'layout'
                output.mkdir()
                with tarfile.open(archive, 'w') as target:
                    first = tarfile.TarInfo('index.json')
                    first.size = 2
                    target.addfile(first, io.BytesIO(b'{}'))
                    bad = tarfile.TarInfo('../escape' if case == 'traversal' else 'index.json')
                    if case == 'link':
                        bad.name = 'oci-layout'
                        bad.type = tarfile.SYMTYPE
                        bad.linkname = '/etc/passwd'
                    target.addfile(bad)
                with self.assertRaises(ValueError):
                    extract_layout(archive, output)
                self.assertEqual(list(output.iterdir()), [])

    def test_digest_addressed_candidate_only(self):
        digest = 'sha256:' + 'a' * 64
        self.assertEqual(destination('mount', digest),
                         'ghcr.io/appmana/seaweedfs-mount:candidate-' + 'a' * 64)
        for component, bad in [('other', digest), ('mount', 'latest'),
                               ('mount', 'sha256:abcd'), ('mount', digest + ':release')]:
            with self.assertRaises(ValueError):
                destination(component, bad)
