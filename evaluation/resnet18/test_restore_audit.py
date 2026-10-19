"""Run inside the existing training image before a cloud audit run."""
import importlib.util
import sys
import tempfile
import types
import unittest
from pathlib import Path
from unittest.mock import Mock, patch

from restore_audit import RestoreAudit, fingerprint


@unittest.skipUnless(importlib.util.find_spec('torch'), 'PyTorch required: run in training image')
class TensorAuditTests(unittest.TestCase):
    def test_tensor_digest_binds_shape_dtype_values(self):
        import torch
        x = torch.tensor([1., 2.])
        self.assertEqual(fingerprint(x), fingerprint(x.clone()))
        for other in (x.double(), x.reshape(1, 2), x + 1):
            self.assertNotEqual(fingerprint(x), fingerprint(other))

    def test_sgd_and_adamw_restore_update_and_corruption(self):
        import torch
        for optimizer_type in (torch.optim.SGD, torch.optim.AdamW):
            with self.subTest(optimizer=optimizer_type.__name__), tempfile.TemporaryDirectory() as temp:
                model = torch.nn.Linear(2, 1)
                kwargs = {'momentum': .9} if optimizer_type is torch.optim.SGD else {}
                optimizer = optimizer_type(model.parameters(), lr=.01, **kwargs)
                x, y = torch.ones(2, 2), torch.zeros(2, 1)
                for _ in range(4):
                    optimizer.zero_grad()
                    (model(x) - y).square().mean().backward()
                    optimizer.step()
                path = Path(temp) / 'latest.pt'
                rng = {'torch_cpu': torch.get_rng_state()}
                torch.save(dict(models=[dict(state_dict=model.state_dict())],
                                optimizers=[dict(state_dict=optimizer.state_dict())],
                                step_counts=[4], rng_states=rng), path)
                saved = torch.load(path, weights_only=False)
                restored = torch.nn.Linear(2, 1)
                restored.load_state_dict(saved['models'][0]['state_dict'])
                opt = optimizer_type(restored.parameters(), lr=.01, **kwargs)
                opt.load_state_dict(saved['optimizers'][0]['state_dict'])
                torch.set_rng_state(rng['torch_cpu'])
                backend = types.ModuleType('fluidcr.backends.pytorch')
                backend._checkpoint_load_path = lambda: str(path)
                backend._collect_rng_states = lambda: {'torch_cpu': torch.get_rng_state()}
                package = types.ModuleType('fluidcr.backends')
                package.pytorch = backend
                with patch.dict(sys.modules, {'fluidcr': types.ModuleType('fluidcr'),
                                              'fluidcr.backends': package,
                                              'fluidcr.backends.pytorch': backend}):
                    events = Mock()
                    audit = RestoreAudit(restored, opt, 4, events)
                    for step in range(5, 8):
                        audit.before(x, y)
                        opt.zero_grad()
                        (restored(x) - y).square().mean().backward()
                        audit.gradients()
                        opt.step()
                        audit.after(step)
                        optimizer.zero_grad()
                        (model(x) - y).square().mean().backward()
                        optimizer.step()
                        self.assertEqual(fingerprint(restored.state_dict()), fingerprint(model.state_dict()))
                        self.assertEqual(fingerprint(opt.state_dict()), fingerprint(optimizer.state_dict()))
                    self.assertEqual(audit.remaining, 0)
                    with self.assertRaisesRegex(RuntimeError, 'state mismatch'):
                        RestoreAudit(restored, opt, 4, events)


if __name__ == '__main__':
    if importlib.util.find_spec('torch') is None:
        raise SystemExit('PyTorch missing; smoke test NOT passed')
    unittest.main()
