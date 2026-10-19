"""Summarize saved audit events; missing evidence never counts as a pass."""
import argparse
import json
from pathlib import Path


def summarize(events):
    rows = list(events)
    def valid(e):
        if e.get('passed') is not True or not e.get('attempt_id'):
            return False
        if e.get('event') == 'restore_audit':
            return e.get('checks') == dict(model=True, optimizer=True, rng=True, step=True)
        return all(e.get(k) is True for k in ('gradients_finite', 'gradients_nonzero', 'parameters_changed'))
    failures = [e for e in rows if e.get('event') in ('restore_audit', 'update_audit') and not valid(e)]
    restores = {}
    for e in rows:
        if e.get('event') == 'restore_audit':
            restores[(e.get('rank'), e.get('attempt_id'))] = e
    results = []
    for (rank, attempt), restore in restores.items():
        updates = {e['global_step'] for e in rows
                   if e.get('event') == 'update_audit' and e.get('rank') == rank
                   and e.get('attempt_id') == attempt and valid(e)}
        expected = set(range(restore['global_step'] + 1, restore['global_step'] + 4))
        results.append({'rank': rank, 'attempt_id': attempt, 'restored_step': restore['global_step'],
                        'state_passed': valid(restore),
                        'three_updates_passed': expected.issubset(updates)})
    passed = (not failures and {r['rank'] for r in results} == {0, 1}
              and all(r['state_passed'] and r['three_updates_passed'] for r in results))
    return {'rank_local_audit_passed': passed, 'failures': failures, 'restores': results,
            'scope': 'checkpoint equality and three local updates, NOT full DDP numerical equivalence'}


if __name__ == '__main__':
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('evidence', type=Path)
    args = parser.parse_args()
    path = args.evidence / 'monitor-events.jsonl'
    result = summarize(json.loads(line) for line in path.read_text().splitlines() if line.strip())
    print(json.dumps(result, indent=2))
    raise SystemExit(0 if result['rank_local_audit_passed'] else 2)
