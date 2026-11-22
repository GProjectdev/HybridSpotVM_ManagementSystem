"""CPU-only replay primitives. No cloud clients, subprocesses or resource mutations.

Observed-event reconstruction is distinct from a predictive stateful simulation.
These guards validate a model's event receipts, not real provider fencing or files.
"""
from dataclasses import dataclass, field
from decimal import Decimal


def number(value):
    x = Decimal(str(value))
    if not x.is_finite():
        raise ValueError('finite numeric value required')
    return x


def integrate_price(start, end, points):
    """Integrate an explicitly supplied price scenario over [start,end).

    points=(effective_time,USD_per_hour). No missing-prefix extrapolation,
    duplicate timestamps, negative prices, invoice rounding or hidden minimums.
    """
    start, end = number(start), number(end)
    curve = [(number(t), number(p)) for t, p in points]
    if end < start or not curve or curve[0][0] > start:
        raise ValueError('complete, nonnegative lifetime and price prefix required')
    if any(p < 0 for _, p in curve) or any(a[0] >= b[0] for a, b in zip(curve, curve[1:])):
        raise ValueError('ordered unique times and nonnegative prices required')
    price = next(p for t, p in reversed(curve) if t <= start)
    total, at = Decimal(0), start
    for time, rate in curve:
        if time <= start:
            continue
        if time >= end:
            break
        total += (time-at)*price/3600
        at, price = time, rate
    return total + (end-at)*price/3600


def interval_union(intervals):
    """Wall-time union; overlapping CP/export/restore may not be added twice."""
    ordered = sorted((number(a), number(b)) for a, b in intervals)
    if any(b < a for a, b in ordered):
        raise ValueError('negative interval')
    total, last = Decimal(0), None
    for a, b in ordered:
        if last is None:
            last = [a, b]
        elif a <= last[1]:
            last[1] = max(last[1], b)
        else:
            total += last[1]-last[0]
            last = [a, b]
    return total + (last[1]-last[0] if last else 0)


@dataclass(frozen=True)
class Round:
    uid: str
    checkpoint_id: str
    world_uid: str
    generation: int
    saved_steps: tuple
    source_pods: tuple
    started: Decimal
    durable: Decimal
    evidence: str


@dataclass
class World:
    """Event-level full-world model, explicitly not a Kubernetes safety oracle."""
    world_uid: str
    pods: tuple
    steps: list = field(default_factory=list)
    generation: int = 0
    rounds: dict = field(default_factory=dict)
    operation: dict | None = None
    at: Decimal = Decimal(0)
    total_rollback: list = field(default_factory=list)
    hold_until: dict = field(default_factory=dict)
    verified_operations: list = field(default_factory=list)

    def __post_init__(self):
        if not self.world_uid or not self.pods or len(set(self.pods)) != len(self.pods) or not all(self.pods):
            raise ValueError('unique named world/rank identities required')
        if not self.steps:
            self.steps = [0] * len(self.pods)
        if len(self.steps) != len(self.pods) or any(type(v) is not int or v < 0 for v in self.steps):
            raise ValueError('full rank step vector required')
        self.total_rollback = [0] * len(self.pods)

    def tick(self, at):
        at = number(at)
        if at < self.at:
            raise ValueError('events must be causal and time ordered')
        self.at = at

    def commit(self, at, rank, pod, step):
        self.tick(at)
        if type(rank) is not int or not 0 <= rank < len(self.pods) or pod != self.pods[rank]:
            raise ValueError('wrong rank/current Pod identity')
        if type(step) is not int or step < self.steps[rank]:
            raise ValueError('rollback requires explicit full-world resume')
        if self.operation and self.operation.get('fenced') and not self.operation.get('resumed'):
            raise ValueError('source cannot progress after fencing')
        if self.operation and self.operation.get('resumed') and step > self.steps[rank]:
            samples = self.operation['progress_samples'][rank]
            if not samples or samples[-1][0] < self.at:
                samples.append((self.at, step))
        self.steps[rank] = step

    def save_round(self, round):
        if round.uid in self.rounds and self.rounds[round.uid] != round:
            raise ValueError('immutable round identity changed')
        if (not round.uid or not round.checkpoint_id or not round.evidence
            or round.world_uid != self.world_uid or round.generation < 1
            or len(round.saved_steps) != len(self.pods) or len(set(round.saved_steps)) != 1
            or len(round.source_pods) != len(self.pods) or len(set(round.source_pods)) != len(self.pods)
            or not all(round.source_pods) or any(type(s) is not int or s < 0 for s in round.saved_steps)
            or round.durable < round.started or round.durable > self.at):
            raise ValueError('complete, same-step, durable, historical full-world round required')
        self.rounds[round.uid] = round

    def latest_round(self):
        candidates = [r for r in self.rounds.values() if r.durable <= self.at]
        if not candidates:
            raise ValueError('no durable full-world round; never invent a checkpoint')
        return sorted(candidates, key=lambda r: (-r.durable, r.checkpoint_id))[0]

    def begin(self, at, operation, target_pods, replacements, planned):
        self.tick(at)
        if self.operation or not operation or type(planned) is not bool or len(target_pods) != len(self.pods) or len(set(target_pods)) != len(self.pods):
            raise ValueError('one operation and complete new world required')
        if set(target_pods) & set(self.pods) or not all(target_pods):
            raise ValueError('new full-group Pod identities required')
        if not replacements or any(m not in ('Spot', 'OnDemand') for m in replacements.values()):
            raise ValueError('explicit replacement identities and markets required')
        self.operation = {'uid': operation, 'targets': tuple(target_pods), 'replacements': dict(replacements),
                          'planned': bool(planned), 'started': self.at, 'ready': None, 'round': None,
                          'fenced': False, 'prepared': False, 'resumed': False}

    def require_operation(self, operation):
        if not self.operation or self.operation['uid'] != operation:
            raise ValueError('operation UID mismatch')
        return self.operation

    def targets_ready(self, at, operation):
        self.tick(at)
        self.require_operation(operation)['ready'] = self.at

    def pin_round(self, at, operation, uid):
        self.tick(at)
        op = self.require_operation(operation)
        r = self.rounds.get(uid)
        if r is None or r.durable > self.at or (op['round'] is not None and op['round'] != uid):
            raise ValueError('exact immutable durable round required')
        if op['planned'] and (op['ready'] is None or r.started < op['ready'] or r.source_pods != self.pods):
            raise ValueError('planned round must start after target readiness in current source world')
        op['round'] = uid

    def fence(self, at, operation, source_pods):
        self.tick(at)
        op = self.require_operation(operation)
        if op['round'] is None or tuple(source_pods) != self.pods:
            raise ValueError('pinned round and exact current source fence receipt required')
        op['fenced'] = True

    def prepare(self, at, operation):
        self.tick(at)
        op = self.require_operation(operation)
        if not op['fenced'] or op['ready'] is None or op['round'] is None:
            raise ValueError('fence, capacity, and durable round gates required')
        op['prepared'] = True

    def resume(self, at, operation):
        self.tick(at)
        op = self.require_operation(operation)
        if not op['prepared'] or op['resumed']:
            raise ValueError('exactly one prepared resume required')
        saved = self.rounds[op['round']].saved_steps
        rollback = [max(0, s-c) for s, c in zip(self.steps, saved)]
        self.total_rollback = [a+b for a, b in zip(self.total_rollback, rollback)]
        op.update(resumed=True, saved=tuple(saved), high_water=tuple(self.steps), rollback=rollback,
                  progress_samples=[[] for _ in self.pods])
        self.steps, self.pods = list(saved), op['targets']
        self.generation += 1

    def verify(self, at, operation):
        self.tick(at)
        op = self.require_operation(operation)
        if (not op['resumed'] or any(s < saved+2 for s, saved in zip(self.steps, op['saved']))
            or any(len(samples) < 2 for samples in op['progress_samples'])):
            raise ValueError('at least two post-resume committed updates per rank required')
        for provider, market in op['replacements'].items():
            if market == 'OnDemand':
                self.hold_until[provider] = self.at+3600
        record = dict(op, verified_at=self.at)
        self.verified_operations.append(record)
        self.operation = None
        return record

    def held_on_demand(self, at, currently_owned_ready):
        self.tick(at)
        return {uid: until for uid, until in self.hold_until.items() if self.at < until and uid in currently_owned_ready}


def trace_value(trace, start_hour, seconds):
    """One-hour native clock; fail right-censored, never wrap/zero-fill."""
    if type(start_hour) is not int or start_hour < 0 or number(seconds) < 0:
        raise ValueError('valid native trace origin required')
    hour = start_hour + int(number(seconds)//3600)
    if hour >= len(trace):
        raise EOFError('RIGHT_CENSORED')
    rate = number(trace[hour])
    if rate < 0:
        raise ValueError('negative hazard')
    return rate
