#!/usr/bin/env python3
"""Renders `go test -bench` output from bench/ as Markdown tables.

Each row is one operation; each column one way of running it. Cells show the
median latency over all runs, its ratio to the original OpenFGA, and SQL
statements (and, for pgx, round trips) per operation.

usage: scripts/report.py bench.txt > benchmarks/tables.md
"""
import collections
import re
import statistics
import sys

pat = re.compile(r'^Benchmark(\S+?)(?:-\d+)?\s+\d+\s+(.*)$')
rows = collections.defaultdict(lambda: collections.defaultdict(list))
order = []
for line in open(sys.argv[1]):
    m = pat.match(line)
    if not m:
        continue
    name, metrics = m.groups()
    parts = name.split('/')
    kind, dialect, op, mode = parts[0], parts[1], '/'.join(parts[2:-1]), parts[-1]
    key = (kind, dialect, op)
    if key not in rows:
        order.append(key)
    rows[key][mode].append(dict((u, float(v)) for v, u in re.findall(r'([\d.]+) ([\w/]+)', metrics)))


def med(samples, unit):
    vals = [s[unit] for s in samples if unit in s]
    return statistics.median(vals) if vals else None


def fmt_ns(ns):
    if ns >= 1e6:
        return f"{ns / 1e6:.2f} ms"
    return f"{ns / 1e3:.0f} µs"


MODES = ['openfga', 'atomicfga-in-tx', 'atomicfga-tx', 'atomicfga-pgx-in-tx', 'atomicfga-pgx-tx']
TITLES = {
    'openfga': 'OpenFGA',
    'atomicfga-in-tx': 'atomicfga, in tx',
    'atomicfga-tx': 'atomicfga, tx per op',
    'atomicfga-pgx-in-tx': 'pgx, in tx',
    'atomicfga-pgx-tx': 'pgx, tx per op',
}

groups = collections.OrderedDict()
for key in order:
    groups.setdefault((key[0], key[1]), []).append(key)

for (kind, dialect), keys in groups.items():
    modes = [m for m in MODES if any(m in rows[k] for k in keys)]
    print(f"### {kind} — {dialect}\n")
    print("| operation | " + " | ".join(TITLES[m] for m in modes) + " |")
    print("|---|" + "---:|" * len(modes))
    for key in keys:
        base = med(rows[key].get('openfga', []), 'ns/op')
        cells = []
        for m in modes:
            samples = rows[key].get(m)
            if not samples:
                cells.append('')
                continue
            ns = med(samples, 'ns/op')
            cell = fmt_ns(ns)
            if m != 'openfga' and base:
                cell += f" ({ns / base:.2f}×)"
            st, trips = med(samples, 'stmts/op'), med(samples, 'trips/op')
            if st is not None:
                cell += f"<br>{st:.1f} q"
                if trips is not None and 'pgx' in m and trips < st - 0.05:
                    cell += f" / {trips:.1f} rt"
            cells.append(cell)
        print(f"| {key[2]} | " + " | ".join(cells) + " |")
    print()
