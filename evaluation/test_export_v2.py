import csv
import json
from pathlib import Path
import tempfile
import unittest

try:
    from . import export_v2
except ImportError:
    import export_v2


class V2ExportTest(unittest.TestCase):
    def test_four_rank_export_retains_rank2_rank3_and_refuses_overwrite(self):
        with tempfile.TemporaryDirectory() as temp:
            root=Path(temp);src=root/'in.jsonl';out=root/'out'
            events=[dict(run_id='four',timestamp='2026-11-18T07:00:00Z',event='step',global_step=1,rank=r,world_size=4,attempt_id='attempt',pod_uid='pod'+str(r)) for r in range(4)]
            src.write_text('\n'.join(json.dumps(x) for x in events+events))
            self.assertEqual(export_v2.export(src,out,'four',world_size=4),{r:1 for r in range(4)})
            before=(out/'rank3_training.csv').read_text()
            with self.assertRaisesRegex(ValueError,'preserve'):export_v2.export(src,out,'four',world_size=4)
            self.assertEqual((out/'rank3_training.csv').read_text(),before)
            events[3]['world_size']=2;src.write_text('\n'.join(json.dumps(x) for x in events))
            with self.assertRaisesRegex(ValueError,'four-rank'):export_v2.export(src,root/'invalid','four',world_size=4)
            self.assertFalse((root/'invalid').exists())
    def test_dedup_keeps_replay_attempts_and_excludes_unvalidated_payload(self):
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp)
            first = dict(run_id="pilot", timestamp="2026-11-17T01:00:00.123456789Z",
                         event="step", global_step=8, rank=0, attempt_id="first",
                         pod_uid="pod-1", node="node-1", secret="must-not-export")
            replay = dict(first, attempt_id="second", pod_uid="pod-2")
            (root / "in.jsonl").write_text("\n".join(json.dumps(x) for x in [first, first, replay]))
            self.assertEqual(export_v2.export(root / "in.jsonl", root / "out", "pilot"), {0: 2, 1: 0})
            text = (root / "out/rank0_training.csv").read_text()
            self.assertNotIn("must-not-export", text)
            with (root / "out/rank0_training.csv").open() as handle:
                rows = list(csv.DictReader(handle))
            self.assertEqual([x["attempt_id"] for x in rows], ["first", "second"])
            self.assertEqual(rows[0]["timestamp_utc"], first["timestamp"])
            self.assertEqual(rows[0]["instance_id"], "")  # missing evidence is not fabricated

            export_v2.export(root / "in.jsonl", root / "mapped", "pilot", "policy-uid", {"node-1": "i-1234"})
            with (root / "mapped/rank0_training.csv").open() as handle:
                mapped = list(csv.DictReader(handle))
            self.assertEqual(mapped[0]["instance_id"], "i-1234")
            self.assertEqual(mapped[0]["policy_id"], "policy-uid")

    def test_wrong_run_fails_before_output_is_created(self):
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp)
            (root / "in.jsonl").write_text(json.dumps(dict(run_id="other", rank=0,
                global_step=1, event="step", timestamp="2026-11-17T01:00:00Z")))
            with self.assertRaises(ValueError):
                export_v2.export(root / "in.jsonl", root / "out", "pilot")
            self.assertFalse((root / "out").exists())
