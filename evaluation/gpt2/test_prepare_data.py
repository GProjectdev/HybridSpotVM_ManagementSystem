import json
from pathlib import Path
import sys
import tempfile
import types
import unittest
from unittest.mock import patch
import prepare_data as p

class PrepareSubsetTests(unittest.TestCase):
    def test_explicit_subset_keeps_full_default_and_validation(self):
        with tempfile.TemporaryDirectory() as tmp:
            r=Path(tmp);model=r/'model';model.mkdir();(model/'tokenizer.json').write_text('{}');raw=r/'raw';raw.mkdir()
            (raw/'train-0.parquet').write_text('training source');(raw/'validation-0.parquet').write_text('validation source')
            tok=types.SimpleNamespace(eos_token_id=0,encode=lambda text,add_special_tokens:[1,2,3])
            fake=types.SimpleNamespace(AutoTokenizer=types.SimpleNamespace(from_pretrained=lambda *a,**kw:tok))
            rows=lambda paths:iter(['row']*(10 if paths[0].name.startswith('train') else 3))
            with patch.dict(sys.modules,{'transformers':fake}),patch.object(p,'parquet_rows',rows):
                full=p.prepare(model,raw,r/'full',4)
                subset=p.prepare(model,raw,r/'subset',4,max_train_blocks=2)
            self.assertEqual(full['splits']['train']['blocks'],10)
            self.assertNotIn('selection',full)
            self.assertEqual(subset['splits']['train']['blocks'],2)
            self.assertEqual(subset['selection']['purpose'],'functional-preflight-only')
            self.assertEqual(full['splits']['validation'],subset['splits']['validation'])
            self.assertEqual(subset['splits']['train']['sha256'],p.file_hash(r/'subset/train.bin'))
            with self.assertRaises(FileExistsError):p.prepare(model,raw,r/'subset',4,max_train_blocks=2)
    def test_invalid_limits_fail_before_creating_output(self):
        for limit in [0,-1,True,1.5]:
            with self.assertRaises(ValueError):p.prepare('/unused','/unused','/unused',4,max_train_blocks=limit)

if __name__=='__main__':unittest.main()
