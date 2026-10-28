"""Independent recomputation from raw *_tx.csv files (does not use analyze.py or summary JSON values)."""
import csv, glob, json, math, os, re, statistics as st, sys
R = sys.argv[1]
def pct(v, p):
    v = sorted(v); i = math.ceil(p / 100 * len(v)) - 1
    return v[max(i, 0)]
out = {}
for exp in ["besu_main", "fabric_main", "besu_period1s", "fabric_majority"]:
    runs = {}
    for f in sorted(glob.glob(f"{R}/{exp}/*_tx.csv")):
        m = re.search(r"_r(\d+)_rep(\d)_tx", f); rate = int(m.group(1))
        rows = list(csv.DictReader(open(f)))
        inw = [r for r in rows if r["in_window"] == "true"]
        ok = [r for r in inw if r["status"] == "ok"]
        fail = [r for r in inw if r["status"] != "ok"]
        thr = sum(1 for r in rows if r["status"] == "ok" and r["commit_in_window"] == "true") / 60
        lat = [float(r["latency_ms"]) / 1000 for r in ok]
        sj = json.load(open(f.replace("_tx.csv", "_summary.json")))
        runs.setdefault(rate, []).append(dict(n_rows=len(rows), sub=len(inw), ok=len(ok), fail=len(fail), sent=len(inw) / 60, thr=thr,
            lat=st.mean(lat) if lat else 0, p95=pct(lat, 95) if lat else 0, skipped=sj["skipped_no_idle_lease"],
            ok_late=sum(1 for r in ok if r["commit_in_window"] == "false"),
            check=dict(thr=abs(thr - sj["throughput_tps"]) < 1e-9, sub=len(inw) == sj["submitted_in_window"],
                       lat=abs((st.mean(lat) if lat else 0) * 1000 - sj["latency_mean_ms"]) < 0.01,
                       p95=abs((pct(lat, 95) if lat else 0) * 1000 - sj["latency_p95_ms"]) < 0.01)))
    out[exp] = runs
tot_rows = sum(r["n_rows"] for e in out.values() for v in e.values() for r in v)
nruns = sum(len(v) for e in out.values() for v in e.values())
print("runs", nruns, "records", tot_rows)
allok = all(all(r["check"].values()) for e in out.values() for v in e.values() for r in v)
print("raw CSV agrees with every summary JSON (sub, thr, mean lat, p95):", allok)
for exp, runs in out.items():
    print("\n==", exp)
    print("rate  sent  thr±sd        mean_lat±sd   p95(mean of runs)  fail%  skipped  ok_committed_after_window")
    for rate in sorted(runs):
        rs = runs[rate]
        thr = [r["thr"] for r in rs]; lat = [r["lat"] for r in rs]
        sub = sum(r["sub"] for r in rs); fail = sum(r["fail"] for r in rs)
        print(f"{rate:4d} {st.mean([r['sent'] for r in rs]):6.1f} {st.mean(thr):6.1f}±{st.stdev(thr):4.1f}  {st.mean(lat):6.2f}±{st.stdev(lat):4.2f}   {st.mean([r['p95'] for r in rs]):6.2f}"
              f"  {100*fail/sub:5.1f}  {sum(r['skipped'] for r in rs):6d}  {sum(r['ok_late'] for r in rs):6d}  thr range {min(thr):.1f}-{max(thr):.1f} lat range {min(lat):.2f}-{max(lat):.2f}")
