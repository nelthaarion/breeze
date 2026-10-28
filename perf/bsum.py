#!/usr/bin/env python3
"""Median-of-runs summary of `go test -bench` output; with two files prints A vs B."""
import re, sys, statistics
def load(path):
    d = {}
    for line in open(path):
        m = re.match(r'^Benchmark(\S+?)(?:-\d+)?\s+\d+\s+([\d.]+) ns/op(?:\s+([\d.]+) B/op)?(?:\s+(\d+) allocs/op)?', line)
        if m:
            e = d.setdefault(m.group(1), {'ns': [], 'b': [], 'a': []})
            e['ns'].append(float(m.group(2)))
            if m.group(3): e['b'].append(float(m.group(3)))
            if m.group(4): e['a'].append(float(m.group(4)))
    return {k: {kk: statistics.median(vv) if vv else None for kk, vv in v.items()} for k, v in d.items()}
a = load(sys.argv[1]); b = load(sys.argv[2]) if len(sys.argv) > 2 else None
flt = sys.argv[3] if len(sys.argv) > 3 else ''
f = lambda x: '-' if x is None else (f'{x:,.1f}' if x < 100 else f'{x:,.0f}')
print(f'{"benchmark":44s} {"A ns/op":>10s} {"B ns/op":>10s} {"delta":>8s}  {"A B/op":>8s} {"B B/op":>8s}  {"A al":>5s} {"B al":>5s}')
for k in sorted(a):
    if flt and not re.search(flt, k): continue
    x = a[k]; y = b.get(k) if b else None
    d = f'{100*(y["ns"]/x["ns"]-1):+.1f}%' if y else ''
    print(f'{k[:44]:44s} {f(x["ns"]):>10s} {f(y["ns"]) if y else "":>10s} {d:>8s}  {f(x["b"]):>8s} {f(y["b"]) if y else "":>8s}  {f(x["a"]):>5s} {f(y["a"]) if y else "":>5s}')
