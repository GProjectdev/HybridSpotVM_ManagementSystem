import json
from pathlib import Path
import tempfile
import unittest
from unittest.mock import patch
from lossless_archive import archive_copy

class ArchiveTests(unittest.TestCase):
    def test_binary_text_and_empty_files_roundtrip_and_originals_remain(self):
        with tempfile.TemporaryDirectory() as t:
            root=Path(t);s=root/'source';s.mkdir();(s/'binary').write_bytes(bytes(range(256))*1024);(s/'empty').touch()
            (s/'text').write_text('학습 원시 기록\n'*100)
            r=archive_copy(s,root/'copy.tar.gz',reserve_bytes=0)
            self.assertEqual(r['file_count'],3);self.assertTrue(r['every_file_roundtrip_sha256_verified'])
            self.assertTrue((s/'binary').exists());self.assertEqual((root/'copy.tar.gz').stat().st_mode&0o777,0o600)
            with self.assertRaises(FileExistsError):archive_copy(s,root/'copy.tar.gz',reserve_bytes=0)
    def test_source_destination_alias_or_symlink_rejected(self):
        with tempfile.TemporaryDirectory() as t:
            s=Path(t)/'s';s.mkdir();(s/'a').write_bytes(b'keep')
            with self.assertRaises(ValueError):archive_copy(s,s/'copy.tar.gz',0)
            (s/'link').symlink_to(s/'a')
            with self.assertRaises(ValueError):archive_copy(s,Path(t)/'copy.tar.gz',0)
    def test_capacity_gate_rejects_before_copy_and_keeps_source(self):
        with tempfile.TemporaryDirectory() as t:
            s=Path(t)/'s';s.mkdir();(s/'a').write_bytes(b'keep')
            with self.assertRaises(ValueError):archive_copy(s,Path(t)/'copy.tar.gz',reserve_bytes=10**30)
            self.assertEqual((s/'a').read_bytes(),b'keep')
if __name__=='__main__':unittest.main()
