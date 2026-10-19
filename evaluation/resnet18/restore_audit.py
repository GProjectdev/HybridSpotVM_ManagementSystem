"""Opt-in rank-local restore audit; not a numerical equivalence proof."""
import hashlib
import json


def fingerprint(value):
    import numpy as np
    import torch
    digest = hashlib.sha256()
    def visit(item):
        if isinstance(item, torch.Tensor):
            tensor = item.detach().cpu().contiguous()
            visit((str(tensor.dtype), list(tensor.shape)))
            digest.update(tensor.reshape(-1).view(torch.uint8).numpy().tobytes())
        elif isinstance(item, np.ndarray):
            visit((str(item.dtype), list(item.shape)))
            digest.update(item.tobytes())
        elif isinstance(item, dict):
            digest.update(b'dict')
            for key in sorted(item, key=lambda k: (type(k).__name__, repr(k))):
                visit(key)
                visit(item[key])
        elif isinstance(item, (tuple, list)):
            digest.update(b'sequence')
            visit(len(item))
            for child in item:
                visit(child)
        else:
            digest.update(json.dumps([type(item).__name__, item], allow_nan=False).encode())
    visit(value)
    return digest.hexdigest()


class RestoreAudit:
    def __init__(self, model, optimizer, step, events):
        import torch
        from fluidcr.backends import pytorch as backend
        self.model, self.events, self.remaining = model, events, 3
        if step == 0:
            events.emit('restore_audit_fresh', step)
            return
        path = backend._checkpoint_load_path()
        # Only read this experiment's trusted checkpoint.
        saved = torch.load(path, map_location='cpu', weights_only=False)
        if len(saved.get('models', [])) != 1 or len(saved.get('optimizers', [])) != 1:
            raise RuntimeError('audit requires one model and optimizer')
        checks = {
            'model': fingerprint(model.state_dict()) == fingerprint(saved['models'][0]['state_dict']),
            'optimizer': fingerprint(optimizer.state_dict()) == fingerprint(saved['optimizers'][0]['state_dict']),
            'rng': fingerprint(backend._collect_rng_states()) == fingerprint(saved['rng_states']),
            'step': saved.get('step_counts') == [step],
        }
        events.emit('restore_audit', step, checks=checks, checkpoint_path=path, passed=all(checks.values()))
        if not all(checks.values()):
            raise RuntimeError('restore state mismatch: ' + repr(checks))

    def before(self, images, labels):
        if self.remaining > 0:
            self.before_hash = fingerprint(dict(self.model.named_parameters()))
            self.input_hash = fingerprint((images, labels))

    def gradients(self):
        import torch
        if self.remaining <= 0:
            return
        grads = [p.grad for p in self.model.parameters() if p.requires_grad and p.grad is not None]
        self.finite = bool(grads) and all(bool(torch.isfinite(g).all()) for g in grads)
        self.nonzero = any(bool(torch.count_nonzero(g)) for g in grads)

    def after(self, step):
        if self.remaining <= 0:
            return
        after = fingerprint(dict(self.model.named_parameters()))
        passed = self.finite and self.nonzero and after != self.before_hash
        self.events.emit('update_audit', step, input_sha256=self.input_hash,
                         parameter_sha256=after, gradients_finite=self.finite,
                         gradients_nonzero=self.nonzero,
                         parameters_changed=after != self.before_hash, passed=passed)
        self.remaining -= 1
        if not passed:
            raise RuntimeError('audit found missing/nonfinite parameter update')
