#!/usr/bin/env python3
"""
Measures per-transaction gas for complete lease lifecycles on a local Ethereum node (anvil).

Every figure is read from the `gasUsed` field of the mined transaction receipt, i.e. it
includes the 21,000 intrinsic cost, calldata cost and execution cost exactly as a user
would pay on an EVM chain with the same hardfork rules.

Usage:  python3 scripts/measure_gas.py [--hardfork cancun] [--out results]
        (from the project root, after `forge build`)
Output: <out>/gas_receipts.csv      one row per transaction
        <out>/gas_summary.json      per-function statistics and per-scenario totals
The defaults (Cancun rules, results/) produce the figures reported in the manuscript; the fork
check runs the same script with --hardfork shanghai (see scripts/gas_fork_check.sh).
"""
import argparse
import csv
import json
import os
import statistics
import subprocess
import sys
import time

FOUNDRY = os.environ.get("FOUNDRY_BIN", "/opt/foundry")
CAST, FORGE, ANVIL = (os.path.join(FOUNDRY, b) for b in ("cast", "forge", "anvil"))
RPC = "http://127.0.0.1:8545"
MNEMONIC = "test test test test test test test test test test test junk"  # anvil default
DAY = 86_400
PERIOD = 30 * DAY
N_PERIODS = 12
RENT = 500 * 10**6
DEPOSIT = 1_000 * 10**6
WINDOWS = (14 * DAY, 14 * DAY, 30 * DAY)  # proposal, challenge, arbitration

ATT_T = "(address,bytes32,uint8,bytes32,uint64,uint256)"
rows = []


def sh(*args):
    r = subprocess.run(args, capture_output=True, text=True)
    if r.returncode != 0:
        sys.exit(f"command failed: {' '.join(args)}\n{r.stderr}")
    return r.stdout.strip()


def key(i):
    return sh(CAST, "wallet", "private-key", "--mnemonic", MNEMONIC, "--mnemonic-index", str(i))


def addr(pk):
    return sh(CAST, "wallet", "address", "--private-key", pk)


def keccak(s):
    return sh(CAST, "keccak", s)


def deploy(path, pk, *ctor):
    args = [FORGE, "create", path, "--rpc-url", RPC, "--private-key", pk, "--broadcast", "--json"]
    if ctor:
        args += ["--constructor-args", *ctor]
    out = json.loads(sh(*args))
    receipt = json.loads(sh(CAST, "receipt", out["transactionHash"], "--rpc-url", RPC, "--json"))
    return out["deployedTo"], int(receipt["gasUsed"], 16)


def send(scenario, label, to, sig, pk, *params):
    out = json.loads(sh(CAST, "send", to, sig, *params, "--rpc-url", RPC, "--private-key", pk, "--json"))
    if out.get("status") not in ("0x1", 1):
        sys.exit(f"tx reverted: {label}")
    gas = int(out["gasUsed"], 16)
    rows.append({"scenario": scenario, "step": label, "function": sig.split("(")[0], "gas_used": gas,
                 "tx_hash": out["transactionHash"]})
    return gas


def advance(seconds):
    sh(CAST, "rpc", "evm_increaseTime", str(seconds), "--rpc-url", RPC)
    sh(CAST, "rpc", "evm_mine", "--rpc-url", RPC)


def now():
    return int(sh(CAST, "block", "latest", "--field", "timestamp", "--rpc-url", RPC))


def attestation(registry, issuer_pk, subject, did, role, prop):
    att = f"({subject},{did},{role},{prop},{now() + 365 * DAY},{int(keccak("nonce:" + subject + ":" + str(role)), 16)})"
    digest = sh(CAST, "call", registry, f"digest({ATT_T})(bytes32)", att, "--rpc-url", RPC)
    sig = sh(CAST, "wallet", "sign", "--no-hash", "--private-key", issuer_pk, digest)
    return att, sig


def new_lease(scenario, ctx, rent_periods_paid):
    """Deploys a lease and runs it up to the lease end; returns the lease address."""
    k = ctx["keys"]
    a = ctx["addrs"]
    terms = (f"({a['landlord']},{a['tenant']},{a['arbitrator']},{ctx['token']},{ctx['property']},"
             f"{RENT},{DEPOSIT},{PERIOD},{N_PERIODS},{WINDOWS[0]},{WINDOWS[1]},{WINDOWS[2]})")
    lease, g = deploy("src/RentalAgreement.sol:RentalAgreement", k["landlord"], terms, ctx["registry"])
    rows.append({"scenario": scenario, "step": "deploy lease", "function": "constructor", "gas_used": g,
                 "tx_hash": ""})

    for who, did, role, prop in (("landlord", ctx["landlord_did"], 1, ctx["property"]),
                                 ("tenant", ctx["tenant_did"], 2, "0x" + "00" * 32)):
        att, sig = attestation(ctx["registry"], k["issuer"], a[who], did, role, prop)
        send(scenario, f"bindIdentity ({who})", lease, f"bindIdentity({ATT_T},bytes)", k[who], att, sig)

    # one-off unlimited allowance; EIP-2612 permit would remove this transaction
    send(scenario, "approve (token, one-off)", ctx["token"], "approve(address,uint256)", k["tenant"], lease,
         str(2**256 - 1))
    send(scenario, "lockDeposit", lease, "lockDeposit()", k["tenant"])

    for i in range(rent_periods_paid):
        send(scenario, f"payRent (period {i + 1})", lease, "payRent()", k["tenant"])
        advance(PERIOD)
    remaining = N_PERIODS - rent_periods_paid
    if remaining:
        advance(PERIOD * remaining)
    return lease


def withdraw_all(scenario, lease, keys, addrs):
    """Pull-pattern settlement: each party with a non-zero share withdraws it."""
    for who in ("landlord", "tenant"):
        amount = int(sh(CAST, "call", lease, "withdrawable(address)(uint256)", addrs[who], "--rpc-url", RPC)
                     .split()[0])
        if amount:
            send(scenario, f"withdraw ({who})", lease, "withdraw()", keys[who])


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--hardfork", default="cancun")
    ap.add_argument("--out", default="results")
    args = ap.parse_args()
    os.makedirs(args.out, exist_ok=True)
    anvil = subprocess.Popen([ANVIL, "--silent", "--hardfork", args.hardfork], stdout=subprocess.DEVNULL)
    time.sleep(2)
    try:
        roles = ["governor", "issuer", "landlord", "tenant", "arbitrator"]
        keys = {r: key(i) for i, r in enumerate(roles)}
        addrs = {r: addr(keys[r]) for r in roles}

        registry, g = deploy("src/IdentityRegistry.sol:IdentityRegistry", keys["governor"], addrs["governor"])
        rows.append({"scenario": "setup", "step": "deploy registry", "function": "constructor", "gas_used": g,
                     "tx_hash": ""})
        send("setup", "addIssuer", registry, "addIssuer(address)", keys["governor"], addrs["issuer"])
        token, g = deploy("test/mocks/MockStablecoin.sol:PlainStablecoin", keys["governor"])
        send("setup", "mint test funds", token, "mint(address,uint256)", keys["governor"], addrs["tenant"],
             str(10**15))

        ctx = {"keys": keys, "addrs": addrs, "registry": registry, "token": token,
               "property": keccak("did:example:property:ramallah-unit-12"),
               "landlord_did": keccak("did:example:landlord-001"),
               "tenant_did": keccak("did:example:tenant-001")}

        # S1: punctual tenant, no damage -> immediate settlement with a zero claim
        lease = new_lease("S1 clean", ctx, N_PERIODS)
        send("S1 clean", "proposeSettlement (0, settles)", lease, "proposeSettlement(uint256,bytes32)",
             keys["landlord"], "0", "0x" + "00" * 32)
        withdraw_all("S1 clean", lease, keys, addrs)

        # S2: damage claim accepted by the tenant
        lease = new_lease("S2 accepted claim", ctx, N_PERIODS)
        send("S2 accepted claim", "proposeSettlement (claim)", lease, "proposeSettlement(uint256,bytes32)",
             keys["landlord"], str(300 * 10**6), keccak("ipfs://inspection-report"))
        send("S2 accepted claim", "acceptSettlement", lease, "acceptSettlement()", keys["tenant"])
        withdraw_all("S2 accepted claim", lease, keys, addrs)

        # S3: unchallenged claim finalised after the challenge window
        lease = new_lease("S3 unchallenged claim", ctx, N_PERIODS)
        send("S3 unchallenged claim", "proposeSettlement (claim)", lease, "proposeSettlement(uint256,bytes32)",
             keys["landlord"], str(300 * 10**6), keccak("ipfs://inspection-report"))
        advance(WINDOWS[1] + 1)
        send("S3 unchallenged claim", "finalizeSettlement", lease, "finalizeSettlement()", keys["governor"])
        withdraw_all("S3 unchallenged claim", lease, keys, addrs)

        # S4: disputed claim with arrears (11 of 12 periods paid), arbitrator reduces the award
        lease = new_lease("S4 disputed + arrears", ctx, N_PERIODS - 1)
        send("S4 disputed + arrears", "proposeSettlement (claim)", lease, "proposeSettlement(uint256,bytes32)",
             keys["landlord"], str(300 * 10**6), keccak("ipfs://inspection-report"))
        send("S4 disputed + arrears", "disputeSettlement", lease, "disputeSettlement(bytes32)", keys["tenant"],
             keccak("ipfs://tenant-photos"))
        send("S4 disputed + arrears", "resolveDispute", lease, "resolveDispute(uint256)", keys["arbitrator"],
             str(100 * 10**6))
        withdraw_all("S4 disputed + arrears", lease, keys, addrs)

        # S5: landlord inactive -> tenant reclaims the deposit after the proposal window
        lease = new_lease("S5 landlord inactive", ctx, N_PERIODS)
        advance(WINDOWS[0] + 1)
        send("S5 landlord inactive", "claimAfterTimeout", lease, "claimAfterTimeout()", keys["tenant"])
        withdraw_all("S5 landlord inactive", lease, keys, addrs)
    finally:
        anvil.terminate()

    with open(os.path.join(args.out, "gas_receipts.csv"), "w", newline="") as f:
        w = csv.DictWriter(f, fieldnames=["scenario", "step", "function", "gas_used", "tx_hash"])
        w.writeheader()
        w.writerows(rows)

    by_fn = {}
    for r in rows:
        if r["scenario"] == "setup":
            continue
        by_fn.setdefault(r["function"], []).append(r["gas_used"])
    per_function = {fn: {"n": len(v), "min": min(v), "max": max(v), "mean": round(statistics.mean(v))}
                    for fn, v in by_fn.items()}
    per_scenario = {}
    for r in rows:
        if r["scenario"] != "setup":
            per_scenario[r["scenario"]] = per_scenario.get(r["scenario"], 0) + r["gas_used"]
    setup = {r["step"]: r["gas_used"] for r in rows if r["scenario"] == "setup"}
    summary = {"node": sh(ANVIL, "--version").splitlines()[0], "hardfork": args.hardfork,
               "solc": "0.8.24, optimizer 200 runs", "setup_one_off": setup,
               "per_function": per_function, "per_scenario_total": per_scenario}
    with open(os.path.join(args.out, "gas_summary.json"), "w") as f:
        json.dump(summary, f, indent=2)
    print(json.dumps(summary, indent=2))


if __name__ == "__main__":
    main()
