"""New four-rank preparation contracts; no torch, cluster or AWS operations."""
import unittest
from evaluation.resnet18.train_resnet18 import StepBatchSampler, parse_config


class FourRankDataContract(unittest.TestCase):
    def config(self, model, rank, batch):
        return parse_config([], {'RANK':str(rank),'WORLD_SIZE':'4','EVAL_MODEL':model,
                                 'EVAL_RUN_ID':'v2-offline','EVAL_BATCH_SIZE':str(batch)})

    def test_four_rank_model_batch_configuration(self):
        for model,batch,global_batch in [('gpt2',2,8),('resnet50',16,64)]:
            configs=[self.config(model,r,batch) for r in range(4)]
            self.assertEqual(sum(c.batch_size for c in configs),global_batch)
            self.assertEqual([c.rank for c in configs],list(range(4)))
            self.assertTrue(all(c.world_size==4 and c.seed==42 for c in configs))

    def test_global_sample_order_preserved_by_batch_partition(self):
        for size,batch2 in [(232588,4),(50000,32)]:
            def batches(world,batch):
                return [list(StepBatchSampler(size,batch,world,r,42,0,3)) for r in range(world)]
            two,four=batches(2,batch2),batches(4,batch2//2)
            for step in range(3):
                a=[sample for rank in two for sample in rank[step]]
                b=[sample for rank in four for sample in rank[step]]
                self.assertEqual(a,b)
                self.assertEqual(len(set(b)),2*batch2)

    def test_four_rank_resume_crosses_epoch_without_reordering(self):
        for batch in (2,16):
            size=batch*4*5+3 # full epochs and deliberately dropped tail
            for rank in range(4):
                full=list(StepBatchSampler(size,batch,4,rank,42,0,12))
                resumed=list(StepBatchSampler(size,batch,4,rank,42,4,12))
                self.assertEqual(full[4:],resumed)

    def test_invalid_four_rank_identity_rejected(self):
        with self.assertRaises(SystemExit):
            self.config('gpt2',4,2)


if __name__=='__main__': unittest.main()
