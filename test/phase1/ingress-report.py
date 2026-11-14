import datetime
import json
import re
import sys
from pathlib import Path

def timestamp(value):
    return datetime.datetime.fromisoformat(re.sub(r'(\.\d{6})\d+', r'\1', value).replace('Z', '+00:00'))

def premature(arrivals, healthy):
    return sum(a['id'] not in healthy or timestamp(a['time']) <= timestamp(healthy[a['id']]) for a in arrivals)

def lines(path):
    return [json.loads(line) for line in path.read_text().splitlines()]

def main(path):
    requests = lines(path / 'requests.jsonl')
    all_arrivals = lines(path / 'arrivals.jsonl')
    arrivals = [a for a in all_arrivals if a['request']]
    events = lines(path / 'events.jsonl')
    healthy = {}
    unhealthy = {}
    for event in events:
        if event.get('Action') in ('health_status: healthy', 'health_status: unhealthy'):
            ident = event['Actor']['ID'][:12]
            t = datetime.datetime.fromtimestamp(event['timeNano'] / 1e9, datetime.timezone.utc).isoformat()
            target = healthy if event['Action'].endswith('healthy') and not event['Action'].endswith('unhealthy') else unhealthy
            target.setdefault(ident, t)
    marks = {name: timestamp((path / (name + '-start')).read_text().strip()) for name in ('delayed', 'bad', 'control')}
    gated = [r for r in requests if timestamp(r['time']) < marks['control']]
    failures = sum(r.get('status') != 200 for r in gated)
    delayed = [a for a in arrivals if a['revision'] == 'delayed' and marks['delayed'] <= timestamp(a['time']) < marks['bad']]
    bad = [a for a in arrivals if a['revision'] == 'bad' and marks['bad'] <= timestamp(a['time']) < marks['control']]
    control = [a for a in arrivals if a['revision'] == 'bad' and timestamp(a['time']) >= marks['control']]
    baseline = [a for a in arrivals if a['revision'] == 'old' and timestamp(a['time']) < marks['delayed']]
    retained = [a for a in arrivals if a['revision'] == 'delayed' and marks['bad'] <= timestamp(a['time']) < marks['control']]
    rollback = json.loads((path / 'rollback.json').read_text())[0]['UpdateStatus']['State']
    direct = (path / 'direct.txt').read_text().strip() if (path / 'direct.txt').exists() else ''
    direct_id = direct.split()[-1] if direct.startswith('bad ') else ''
    direct_arrivals = [a for a in all_arrivals if not a['request'] and a['id'] == direct_id]
    print(json.dumps({'requests_gated': len(gated), 'failures_gated': failures, 'baseline_hits': len(baseline), 'delayed_hits': len(delayed), 'delayed_premature': premature(delayed, healthy), 'first_delayed_arrival': delayed[0] if delayed else None, 'retained_hits_during_bad': len(retained), 'bad_hits': len(bad), 'control_hits': len(control), 'first_control_arrival': control[0] if control else None, 'direct_bad': direct, 'direct_bad_arrival': direct_arrivals[0] if direct_arrivals else None, 'rollback': rollback, 'healthy': healthy, 'unhealthy': unhealthy, 'total_requests': len(requests)}, indent=2))
    if failures or not baseline or not delayed or premature(delayed, healthy) or not retained or bad or not control or direct_id not in unhealthy or rollback != 'rollback_completed':
        raise SystemExit('GATE FAILED OR INCONCLUSIVE: inspect the raw trace')

if __name__ == '__main__': main(Path(sys.argv[1]))
