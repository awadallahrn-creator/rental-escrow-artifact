# Rental escrow protocol — EVM and Hyperledger Fabric implementations, benchmark and data

Research artifact for the manuscript *"A Blockchain-Based Framework for Decentralized Rental
Management: Unifying Identity, Recurring Payments, and Escrow Across Public and Permissioned
Ledgers"* (Journal of Engineering Research, JER-D-26-01274, revised version).

Every table, figure and number in the revised manuscript is generated from the raw data in this
repository by `bench/analyze.py`.

Archived release (Zenodo): https://doi.org/10.5281/zenodo.23639391

## Layout

| Path | Content |
|---|---|
| `src/` | Solidity: `IdentityRegistry.sol` (EIP-712 attestations, issuer governance, revocation), `RentalAgreement.sol` (lease protocol) |
| `test/` | Foundry unit tests, property-based test, stateful invariant suite, test tokens |
| `scripts/measure_gas.py` | Five complete leases on a local EVM node; gas from receipts (`--hardfork`, `--out`) |
| `scripts/gas_fork_check.sh`, `scripts/compare_gas.py` | Gas re-measured under Shanghai rules and compared with the reported Cancun figures |
| `results/` | Gas receipts and summary, Slither report, `gas_fork_check/` (fork-check runs) |
| `fabric/chaincode/` | Go chaincode: `token` and `rental` (same protocol), unit/property/invariant tests, in-memory Fabric test ledger |
| `bench/fabric/` | Fabric 3.1.1 network: 4 SmartBFT orderers, 3 peer organizations, TLS, chaincode-as-a-service |
| `bench/besu/` | Besu 24.12.2 network: 4 QBFT validators |
| `bench/loadgen/` | Load generator (Go) used for both platforms, plus end-to-end functional scenarios |
| `bench/run_sweep.sh`, `bench/pipeline.sh` | Measurement programme |
| `bench/results/` | Raw benchmark data: one CSV row per transaction, one JSON summary per run, end-to-end outputs |
| `bench/analyze.py` | Generates all manuscript tables, figures and numbers (`paper/generated/`, `paper/figures/`) |
| `paper/` | Inputs used by the manuscript: bibliography (`refs.bib`), generated tables and number macros (`generated/`) and the performance figure (`figures/`). The manuscript source itself is not included. |
| `verification_*/` | Logs of the test runs, table regeneration and independent recomputation |

## Protocol in brief

A lease is a state machine: `Created → Active → (SettlementProposed → Disputed →) Settled`.

- **Identity binding.** Each party must present a credential from a trusted issuer stating its role
  and, for the landlord, the property. EVM: EIP-712 attestation checked by `IdentityRegistry`.
  Fabric: X.509 certificate attributes `rental.role`, `rental.did`, `rental.property`; issuer trust,
  expiry and revocation enforced by the MSP.
- **Rent.** Fixed period schedule; at most one period ahead; arrears computed from on-chain records.
- **Settlement.** Unpaid rent deducted automatically; damage claim bound to an evidence hash; tenant
  challenge window; arbitrator can reduce but not increase the claim; deadlines on every waiting state,
  so settlement cannot be stalled by an inactive party; shares withdrawn separately by each party
  (a withdrawal also requires the token to complete the transfer).
- **Identity policy.** Credentials are checked when a party binds. On the EVM the binding then persists;
  on Fabric the MSP also validates the party's certificate on every later transaction.
- **Checked invariants (both platforms, from deposit locking onwards).** I1 escrow balance ≥ liabilities
  (tokens sent to the escrow without a protocol call are surplus: never allocated, never withdrawable);
  I2 deposit conserved;
  I3 landlord receipts bounded; I4 no overpayment; I5 state monotonic.

## Reproducing

### EVM contracts and gas
Requirements: Foundry (`forge`, `anvil`, `cast`); dependencies are vendored under `lib/`.
```bash
forge build
forge test                       # unit, property-based and invariant tests
python3 scripts/measure_gas.py   # -> results/gas_receipts.csv, results/gas_summary.json
slither . --filter-paths "lib/|test/"
```

### Reproducing the Shanghai gas check
The contracts are compiled for Shanghai (`foundry.toml`); the reported gas figures were measured on
Anvil with Cancun rules. The fork check re-measures the five leases under Shanghai rules and once more
under Cancun rules, and compares both runs with `results/gas_receipts.csv`:
```bash
forge build
scripts/gas_fork_check.sh            # -> results/gas_fork_check/rerun/{shanghai,cancun_rerun}/, comparison.txt
```
All receipts are identical across fork rules except `bindIdentity`, whose calldata contains a signed
attestation that differs between runs; its gas varies by 12–24 gas from run to run under either rule
set, so the number of differing `bindIdentity` receipts changes between reruns. The run reported in
the manuscript (98 of 104 receipts identical under Shanghai; six `bindIdentity` receipts differing by
12 gas) is stored in `results/gas_fork_check/reported_2026-10-26/`; a later rerun is in
`results/gas_fork_check/rerun_2026-10-27/`.

### Fabric chaincode
Requirements: Go ≥ 1.24.
```bash
cd fabric/chaincode && go test ./...
```

### Benchmark
Requirements: Fabric 3.1.1 and Besu 24.12.2 release binaries (paths configurable via `FABRIC=` and
`BESU=`), Go, Java 21. Networks run natively on one host, one platform at a time.
```bash
cd bench/loadgen && go build -o /opt/bench/loadgen .
cd .. && besu/network.sh up 2
/opt/bench/loadgen setup -platform besu -leases 1000 -artifacts ../out -state /opt/bench/besunet/bench_state.json
./run_sweep.sh besu besu_main 3 10 25 50 75 100 125 150
./pipeline.sh                    # Besu sensitivity, Fabric main and sensitivity, end-to-end checks
python3 analyze.py               # regenerate manuscript tables and figures
```

Configuration choices (documented in the manuscript, Section 5.3): consensus parameters are taken
unchanged from `fabric-samples` (BFT test network) and the Besu QBFT tutorial; block capacity is set so
that it does not bind (Besu gas limit; Fabric 3.1.1 release-default batch size); the peer
`TimeWindowCheck` filter is enabled as in the Fabric 3.1.1 release configuration.

## Notes on the measurements

- `results/gas_receipts.csv` has 104 receipts: 101 from the five lease scenarios and three one-off
  setup transactions (registry deployment, issuer registration, test-token minting). All scenarios use
  the same landlord account, so only the first payment of S1 (92,420 gas) writes a zero balance slot.
- The load generator skips a scheduled submission when all 1,000 leases have a payment in flight
  (`skipped_no_idle_lease` in each run summary). This occurred only beyond saturation (Besu at 125 and
  150 tx/s, Fabric from 75 tx/s); latency and failure rate then refer to the payments actually sent.
- `verification_2026-10-26/` and `verification_2026-10-27/` contain the logs of the final test runs
  (45 EVM unit tests, 1 property-based test, 5 invariants; 39 Fabric unit tests, property-based test and
  invariant suite) and an independent recomputation of the performance figures from the raw CSV files.

## Licence

Code (contracts, chaincode, tests, scripts, load generator, network configurations): MIT, see
`LICENSE`. Data and generated outputs (`bench/results/`, `results/`, `paper/generated/`,
`paper/figures/`, `verification_*/`): CC-BY-4.0 (https://creativecommons.org/licenses/by/4.0/).
Vendored dependencies under `lib/` keep their own licences.

## Scope

Single landlord–tenant lease. Not independently audited. Benchmark results are bounded by the test host
(2 vCPUs, 8 GB RAM) and exclude inter-node network latency.
