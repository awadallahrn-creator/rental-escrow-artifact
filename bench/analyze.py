#!/usr/bin/env python3
"""
Aggregates raw benchmark, gas and end-to-end results and writes every number used in the
manuscript as LaTeX macros, tables and figures. Nothing in the manuscript's results is typed
by hand: each value is traceable to a file under bench/results/ or results/.

Usage: python3 bench/analyze.py   (from the repository root)
"""
import csv
import glob
import json
import os
import statistics as st
from collections import defaultdict

import matplotlib
matplotlib.use("Agg")
import matplotlib.pyplot as plt  # noqa: E402

ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
RES = os.path.join(ROOT, "bench", "results")
GEN = os.path.join(ROOT, "paper", "generated")
FIG = os.path.join(ROOT, "paper", "figures")
os.makedirs(GEN, exist_ok=True)
os.makedirs(FIG, exist_ok=True)

macros = {}


def macro(name, value):
    macros[name] = value


def fmt(x, nd=1):
    return f"{x:,.{nd}f}".replace(",", "{,}")


def fint(x):
    return f"{int(round(x)):,}".replace(",", "{,}")


# --------------------------------------------------------------------------- performance
def load_exp(exp):
    runs = defaultdict(list)
    for f in glob.glob(os.path.join(RES, exp, "*_summary.json")):
        d = json.load(open(f))
        runs[d["offered_tps"]].append(d)
    return dict(sorted(runs.items()))


def agg(runs):
    rows = []
    for rate, rs in runs.items():
        thr = [r["throughput_tps"] for r in rs]
        mean_lat = [r["latency_mean_ms"] / 1000 for r in rs]
        p95 = [r["latency_p95_ms"] / 1000 for r in rs]
        sub = sum(r["submitted_in_window"] for r in rs)
        ok = sum(r["succeeded_in_window"] for r in rs)
        fail = sum(r["failed_in_window"] + r["submit_errors_in_window"] + r["timed_out_in_window"] for r in rs)
        skipped = sum(r["skipped_no_idle_lease"] for r in rs)
        cpu_nodes = [sum(v for k, v in r["cpu_percent_of_host"].items() if k != "loadgen") for r in rs]
        cpu_client = [r["cpu_percent_of_host"].get("loadgen", 0) for r in rs]
        reasons = defaultdict(int)
        for r in rs:
            for k, v in r["failure_reasons"].items():
                reasons[k] += v
        sd = lambda v: st.stdev(v) if len(v) > 1 else 0.0  # noqa: E731
        sent = [r["send_tps"] for r in rs]
        rows.append(dict(thr_min=min(thr), thr_max=max(thr), lat_min=min(mean_lat), lat_max=max(mean_lat), rate=rate, n=len(rs), sent=st.mean(sent), thr=st.mean(thr), thr_sd=sd(thr), lat=st.mean(mean_lat),
                         lat_sd=sd(mean_lat), p95=st.mean(p95), p95_sd=sd(p95), sub=sub, ok=ok, fail=fail,
                         fail_pct=100 * fail / sub if sub else 0, skipped=skipped,
                         cpu=st.mean(cpu_nodes), cpu_client=st.mean(cpu_client), reasons=dict(reasons)))
    return rows


EXPS = {
    "besu_main": ("Besu (QBFT, 2\\,s block period)", "BesuMain"),
    "fabric_main": ("Fabric (SmartBFT, unanimous endorsement)", "FabricMain"),
    "besu_period1s": ("Besu (QBFT, 1\\,s block period)", "BesuOneSec"),
    "fabric_majority": ("Fabric (SmartBFT, 2-of-3 endorsement)", "FabricMajority"),
}
data = {e: agg(load_exp(e)) for e in EXPS if os.path.isdir(os.path.join(RES, e))}

ROMAN = {10: "Ten", 25: "TwentyFive", 50: "Fifty", 75: "SeventyFive", 100: "Hundred", 125: "HundredTwentyFive",
         150: "HundredFifty"}

for exp, rows in data.items():
    tag = EXPS[exp][1]
    if not rows:
        continue
    peak = max(rows, key=lambda r: r["thr"])
    macro(f"{tag}PeakThr", fmt(peak["thr"]))
    macro(f"{tag}PeakThrSd", fmt(peak["thr_sd"]))
    macro(f"{tag}PeakRate", fint(peak["rate"]))
    macro(f"{tag}PeakCpu", fint(peak["cpu"]))
    macro(f"{tag}PeakLat", fmt(peak["lat"], 2))
    # largest offered load sustained without failures and with throughput within 5% of offered
    sustained = [r for r in rows if r["fail"] == 0 and r["skipped"] == 0 and r["thr"] >= 0.95 * r["rate"]]
    if sustained:
        s = max(sustained, key=lambda r: r["rate"])
        macro(f"{tag}SustRate", fint(s["rate"]))
        macro(f"{tag}SustLat", fmt(s["lat"], 2))
        macro(f"{tag}SustPNinetyFive", fmt(s["p95"], 2))
        macro(f"{tag}SustCpu", fint(s["cpu"]))
    for r in rows:
        k = ROMAN.get(r["rate"])
        if not k:
            continue
        macro(f"{tag}Thr{k}", fmt(r["thr"]))
        macro(f"{tag}Lat{k}", fmt(r["lat"], 2))
        macro(f"{tag}PNinetyFive{k}", fmt(r["p95"], 2))
        macro(f"{tag}Fail{k}", fmt(r["fail_pct"]))
        macro(f"{tag}Cpu{k}", fint(r["cpu"]))
        macro(f"{tag}LatMin{k}", fmt(r["lat_min"], 2))
        macro(f"{tag}LatMax{k}", fmt(r["lat_max"], 2))
        macro(f"{tag}Sent{k}", fmt(r["sent"]))
    macro(f"{tag}Runs", fint(sum(r["n"] for r in rows)))
    macro(f"{tag}Tx", fint(sum(r["sub"] for r in rows)))
    macro(f"{tag}MaxClientCpu", fmt(max(r["cpu_client"] for r in rows)))
    all_reasons = defaultdict(int)
    for r in rows:
        for k, v in r["reasons"].items():
            all_reasons[k] += v
    json.dump(dict(all_reasons), open(os.path.join(GEN, f"failure_reasons_{exp}.json"), "w"), indent=1)
    macro(f"{tag}UnavailFail", fint(sum(v for k, v in all_reasons.items() if "insufficient number of orderers" in k)))
    macro(f"{tag}MvccFail", fint(sum(v for k, v in all_reasons.items() if "MVCC_READ_CONFLICT" in k)))
    tot = sum(all_reasons.values())
    if tot:
        endorse = sum(v for k, v in all_reasons.items() if "endorsement" in k)
        macro(f"{tag}EndorseFailShare", fint(100 * endorse / tot))
        macro(f"{tag}FailTotal", fint(tot))
    over = [r for r in rows if r["rate"] > (sustained and max(x["rate"] for x in sustained) or 0)]
    if over:
        macro(f"{tag}OverThrMin", fmt(min(r["thr"] for r in over)))
        macro(f"{tag}OverThrMax", fmt(max(r["thr"] for r in over)))
        macro(f"{tag}OverLatMin", fmt(min(r["lat"] for r in over)))
        macro(f"{tag}OverLatMax", fmt(max(r["lat"] for r in over)))
        macro(f"{tag}OverFailMin", fmt(min(r["fail_pct"] for r in over)))
        macro(f"{tag}OverFailMax", fmt(max(r["fail_pct"] for r in over)))


def perf_table(exp_list, fname, caption, label):
    lines = [r"\begin{table}[t]", r"\centering", r"\footnotesize", f"\\caption{{{caption}}}", f"\\label{{{label}}}",
             r"\begin{tabular}{@{}rrrrrrr@{}}", r"\toprule",
             r"Offered & Sent & Throughput & Mean latency & p95 latency & Failed & Node CPU \\",
             r"(tx/s) & (tx/s) & (tx/s) & (s) & (s) & (\%) & (\%) \\"]
    for exp in exp_list:
        if exp not in data:
            continue
        lines += [r"\midrule", f"\\multicolumn{{7}}{{@{{}}l}}{{\\emph{{{EXPS[exp][0]}}}}} \\\\", r"\addlinespace[2pt]"]
        for r in data[exp]:
            lines.append(f"{r['rate']} & {r['sent']:.1f} & {r['thr']:.1f} $\\pm$ {r['thr_sd']:.1f} & "
                         f"{r['lat']:.2f} $\\pm$ {r['lat_sd']:.2f} & {r['p95']:.2f} & {r['fail_pct']:.1f} & "
                         f"{r['cpu']:.0f} \\\\")
    lines += [r"\bottomrule", r"\end{tabular}", r"\end{table}"]
    open(os.path.join(GEN, fname), "w").write("\n".join(lines) + "\n")


perf_table(["besu_main", "fabric_main"], "perf_main_table.tex",
           "Performance under fixed offered load (rent payments). Mean $\\pm$ standard deviation over three "
           "repetitions of 60\\,s each; p95 is the mean of the per-run 95th percentiles. \\emph{Sent} falls below "
           "\\emph{Offered} when all 1{,}000 leases already have a payment in flight (a tenant pays one period at a "
           "time); the scheduled submission is then skipped, so beyond saturation latency and failure rate refer to the "
           "payments actually sent. Latency is client-observed submit-to-commit time of committed payments; "
           "\\emph{Failed} is the share of payments sent in the window that were rejected at endorsement or "
           "submission, invalidated at commit, or not committed within the drain period; CPU is the share of total "
           "host capacity used by the blockchain nodes (all cores = 100\\%).", "tab:perf")
perf_table(["besu_period1s", "fabric_majority"], "perf_sens_table.tex",
           "Sensitivity runs (same workload and methodology as Table~\\ref{tab:perf}).", "tab:sens")

# figures
style = {"besu_main": ("o-", "C0"), "fabric_main": ("s-", "C3"), "besu_period1s": ("o--", "C0"),
         "fabric_majority": ("s--", "C3")}
labels = {"besu_main": "Besu, QBFT (2 s blocks)", "fabric_main": "Fabric, SmartBFT (3-of-3 endorsement)",
          "besu_period1s": "Besu, QBFT (1 s blocks)", "fabric_majority": "Fabric, SmartBFT (2-of-3 endorsement)"}
plt.rcParams.update({"font.size": 9})
fig, ax = plt.subplots(1, 2, figsize=(7.4, 3.7))
for exp, rows in data.items():
    if not rows:
        continue
    x = [r["rate"] for r in rows]
    m, c = style[exp]
    a1 = 1 if "main" in exp else 0.6
    thr = [r["thr"] for r in rows]
    thr_lo = [r["thr"] - r["thr_min"] for r in rows]
    thr_hi = [r["thr_max"] - r["thr"] for r in rows]
    ax[0].errorbar(x, thr, yerr=[thr_lo, thr_hi], fmt=m, color=c, ms=4, capsize=2, label=labels[exp], alpha=a1)
    lat = [r["lat"] for r in rows]
    lat_lo = [r["lat"] - r["lat_min"] for r in rows]
    lat_hi = [r["lat_max"] - r["lat"] for r in rows]
    ax[1].errorbar(x, lat, yerr=[lat_lo, lat_hi], fmt=m, color=c, ms=4, capsize=2, label=labels[exp], alpha=a1)
maxrate = max((r["rate"] for rows in data.values() for r in rows), default=150)
ax[0].plot([0, maxrate], [0, maxrate], ":", color="grey", lw=1, label="committed = offered")
ax[0].set_xlabel("Offered load (tx/s)")
ax[0].set_ylabel("Committed throughput (tx/s)")
ax[1].set_xlabel("Offered load (tx/s)")
ax[1].set_ylabel("Mean submit-to-commit latency (s)")
ax[1].set_yscale("log")
for a in ax:
    a.grid(alpha=0.3)
h, l = ax[0].get_legend_handles_labels()
fig.legend(h, l, loc="upper center", ncol=3, fontsize=7, frameon=False)
fig.tight_layout(rect=[0, 0, 1, 0.84])
fig.savefig(os.path.join(FIG, "performance.pdf"))

# --------------------------------------------------------------------------- gas
gas = json.load(open(os.path.join(ROOT, "results", "gas_summary.json")))
pf = gas["per_function"]
names = {"constructor": "Deploy lease contract", "bindIdentity": "\\texttt{bindIdentity} (per party)",
         "approve": "Token \\texttt{approve} (one-off)", "lockDeposit": "\\texttt{lockDeposit}",
         "payRent": "\\texttt{payRent}", "proposeSettlement": "\\texttt{proposeSettlement}",
         "acceptSettlement": "\\texttt{acceptSettlement}", "finalizeSettlement": "\\texttt{finalizeSettlement}",
         "disputeSettlement": "\\texttt{disputeSettlement}", "resolveDispute": "\\texttt{resolveDispute}",
         "claimAfterTimeout": "\\texttt{claimAfterTimeout}", "withdraw": "\\texttt{withdraw} (per party)"}
lines = [r"\begin{table}[t]", r"\centering", r"\small",
         r"\caption{Gas used per transaction in the five lease scenarios of Table~\ref{tab:lifecycle}, read from "
         r"transaction receipts on a local node with Cancun rules (contracts compiled for Shanghai; solc 0.8.24, "
         r"optimiser 200 runs). $n$ is the number of receipts (" + str(sum(v["n"] for v in pf.values())) + r" in total). "
         r"Not listed are the three one-off setup receipts: registry deployment (" + fint(gas["setup_one_off"]["deploy registry"]) +
         r" gas), issuer registration (" + fint(gas["setup_one_off"]["addIssuer"]) + r") and test-token minting. "
         r"Ranges reflect different code paths (a zero versus a positive claim), the first payment to a landlord "
         r"without a token balance (\texttt{payRent}), the withdrawing party (\texttt{withdraw}) and calldata that "
         r"varies between runs (\texttt{bindIdentity}).}",
         r"\label{tab:gas}", r"\begin{tabular}{@{}lrrrr@{}}", r"\toprule",
         r"Transaction & $n$ & Min & Mean & Max \\", r"\midrule"]
for k, label in names.items():
    v = pf[k]
    lines.append(f"{label} & {v['n']} & {v['min']:,} & {v['mean']:,} & {v['max']:,} \\\\".replace(",", "{,}"))
lines += [r"\bottomrule", r"\end{tabular}", r"\end{table}"]
open(os.path.join(GEN, "gas_table.tex"), "w").write("\n".join(lines) + "\n")
for k in ("payRent", "lockDeposit", "constructor", "withdraw", "bindIdentity", "approve"):
    macro(f"Gas{k[0].upper() + k[1:]}Mean", fint(pf[k]["mean"]))
    macro(f"Gas{k[0].upper() + k[1:]}Min", fint(pf[k]["min"]))
    macro(f"Gas{k[0].upper() + k[1:]}Max", fint(pf[k]["max"]))
macro("GasPayRentRatio", fmt(pf["payRent"]["min"] / 21000, 1))
sc = gas["per_scenario_total"]
scen_names = {"S1 clean": "S1: punctual tenant, no damage", "S2 accepted claim": "S2: damage claim accepted",
              "S3 unchallenged claim": "S3: claim unchallenged, finalised after window",
              "S4 disputed + arrears": "S4: one period unpaid, claim disputed, award reduced",
              "S5 landlord inactive": "S5: landlord inactive, tenant reclaims after timeout"}
lines = [r"\begin{table}[t]", r"\centering", r"\small",
         r"\caption{Total gas for a complete 12-period lease, from deployment of the lease contract to the final "
         r"withdrawal, excluding the one-off network setup (Table~\ref{tab:gas}). All scenarios used the same "
         r"landlord account: only S1 contains a first payment to a landlord without a token balance ("
         + fint(pf["payRent"]["max"]) + r" gas); in S2--S5 every payment cost " + fint(pf["payRent"]["min"]) + r" gas.}",
         r"\label{tab:lifecycle}", r"\begin{tabular}{@{}lr@{}}", r"\toprule", r"Scenario & Total gas \\", r"\midrule"]
for k, label in scen_names.items():
    lines.append(f"{label} & {sc[k]:,} \\\\".replace(",", "{,}"))
lines += [r"\bottomrule", r"\end{tabular}", r"\end{table}"]
open(os.path.join(GEN, "lifecycle_table.tex"), "w").write("\n".join(lines) + "\n")
s1 = sc["S1 clean"]
macro("GasLifecycleClean", fint(s1))
macro("GasDeployShare", fint(100 * pf["constructor"]["mean"] / s1))
rows_s1 = [r for r in csv.DictReader(open(os.path.join(ROOT, "results", "gas_receipts.csv"))) if r["scenario"] == "S1 clean"]
rent_s1 = sum(int(r["gas_used"]) for r in rows_s1 if r["function"] == "payRent")
macro("GasRentShare", fint(100 * rent_s1 / s1))
macro("GasRegistryDeploy", fint(gas["setup_one_off"]["deploy registry"]))
macro("GasReceipts", fint(sum(v["n"] for v in pf.values())
                          + len(gas["setup_one_off"])))
# parametric cost of one payRent and of a complete lease (S1); the price grid is an assumption grid, not market data
grid_gwei = [1, 10, 50]
grid_eth = [2000, 4000]
lines = [r"\begin{table}[t]", r"\centering", r"\small",
         r"\caption{Illustrative fee in USD of one \texttt{payRent} transaction (" + fint(pf["payRent"]["min"]) +
         r" gas) and of the complete lease S1 (" + fint(s1) + r" gas, excluding one-off network setup) for assumed "
         r"gas prices and ETH/USD rates. The grid is a set of assumptions for scale, not current market prices; fees "
         r"on a public network vary with demand.}",
         r"\label{tab:fee}", r"\begin{tabular}{@{}r" + "r" * (2 * len(grid_eth)) + r"@{}}", r"\toprule",
         r" & \multicolumn{" + str(len(grid_eth)) + r"}{c}{One \texttt{payRent}} & \multicolumn{" + str(len(grid_eth)) +
         r"}{c}{Complete lease S1} \\",
         r"\cmidrule(lr){2-" + str(1 + len(grid_eth)) + r"}\cmidrule(l){" + str(2 + len(grid_eth)) + "-" + str(1 + 2 * len(grid_eth)) + r"}",
         r"Gas price & " + " & ".join(f"1 ETH = {e:,}".replace(",", "{,}") for e in grid_eth * 2) + r" \\",
         r"(gwei) & " + " & ".join("USD" for _ in grid_eth * 2) + r" \\",
         r"\midrule"]
for g in grid_gwei:
    cells = [f"{pf['payRent']['min'] * g * 1e-9 * e:.3f}" for e in grid_eth] + [f"{s1 * g * 1e-9 * e:,.2f}".replace(",", "{,}") for e in grid_eth]
    lines.append(f"{g} & " + " & ".join(cells) + r" \\")
lines += [r"\bottomrule", r"\end{tabular}", r"\end{table}"]
open(os.path.join(GEN, "fee_table.tex"), "w").write("\n".join(lines) + "\n")

# --------------------------------------------------------------------------- end-to-end
e2e = {}
for f in glob.glob(os.path.join(RES, "e2e", "*_e2e.csv")):
    for r in csv.DictReader(open(f)):
        e2e.setdefault(r["scenario"].split()[0], {})[r["platform"]] = r
E2E_DESC = {
    "S1": "S1 punctual tenant, no damage claim",
    "S2": "S2 damage claim accepted by tenant",
    "S3": "S3 damage claim unchallenged, finalized after window",
    "S4": "S4 one period unpaid; claim disputed; award reduced",
    "S5": "S5 landlord inactive; tenant claims after timeout",
    "S6": "S6 one period unpaid; claim disputed; arbitrator inactive",
    "N1": "N1 expired credential (attestation / certificate)",
    "N2": "N2 credential from untrusted issuer",
    "N3": "N3 revoked attestation (Besu); certificate without role/DID attributes (Fabric)",
    "N4": "N4 rent payment by a non-tenant",
}
if e2e:
    lines = [r"\begin{table}[t]", r"\centering", r"\scriptsize",
             r"\caption{End-to-end functional verification on the deployed test networks. Allocations are deposit shares in "
             r"token units (deposit = 1{,}000, rent = 500 per period); \emph{recorded} is the share stored by the "
             r"contract or chaincode, \emph{withdrawn} the amount actually received. N1--N4 must be rejected.}",
             r"\label{tab:e2e}", r"\begin{tabularx}{\textwidth}{@{}>{\raggedright}Xcccc@{}}", r"\toprule",
             r"Scenario & Specified & Besu: recorded; & Fabric: recorded; & Pass \\",
             r" & L / T & withdrawn (L/T) & withdrawn (L/T) & \\", r"\midrule"]
    passed = total = 0
    order = sorted(e2e, key=lambda k: (k[0] != "S", int(k[1:])))
    for key in order:
        p = e2e[key]
        b, f_ = p.get("besu"), p.get("fabric")
        ok = all(x is not None and x["pass"] == "true" for x in (b, f_))
        passed += ok
        total += 1
        if key.startswith("N"):
            cell = lambda x: x["note"] if x else "--"  # noqa: E731
            lines.append(f"{E2E_DESC[key]} & rejected & {cell(b)} & {cell(f_)} & {'yes' if ok else 'NO'} \\\\")
        else:
            ref = b or f_
            spec = f"{ref['want_landlord']} / {ref['want_tenant']}"
            cell = lambda x: f"{x['got_landlord']}/{x['got_tenant']}; {x['withdrawn_landlord']}/{x['withdrawn_tenant']}" if x else "--"  # noqa: E731
            lines.append(f"{E2E_DESC[key]} & {spec} & {cell(b)} & {cell(f_)} & {'yes' if ok else 'NO'} \\\\")
    lines += [r"\bottomrule", r"\end{tabularx}", r"\end{table}"]
    open(os.path.join(GEN, "e2e_table.tex"), "w").write("\n".join(lines) + "\n")
    macro("EtoEPassed", str(passed))
    macro("EtoETotal", str(total))

# --------------------------------------------------------------------------- tests
tf = os.path.join(RES, "tests.json")
if os.path.exists(tf):
    t = json.load(open(tf))
    for k, v in t.items():
        macro(k, str(v))

with open(os.path.join(GEN, "numbers.tex"), "w") as f:
    f.write("% Generated by bench/analyze.py -- do not edit by hand.\n")
    for k in sorted(macros):
        f.write(f"\\newcommand{{\\{k}}}{{{macros[k]}}}\n")
print(f"{len(macros)} macros; experiments: {', '.join(f'{e}({sum(r['n'] for r in rows)} runs)' for e, rows in data.items())}")
