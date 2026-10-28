# Verification — 2026-10-27 (run in a clean clone of this repository)

| Check | Command | Result |
|---|---|---|
| EVM tests | `forge test` | 51 passed, 0 failed: 45 unit tests, 1 property-based test (1,000 runs), 5 invariants (256 runs × depth 200) — `forge_test.log` |
| Fabric tests | `cd fabric/chaincode && go test ./... -v` | PASS: 39 unit tests, property-based test, invariant suite — `go_test.log` |
| Table regeneration | `python3 bench/analyze.py` | `paper/generated/` identical to the inputs of the revised manuscript (empty diff, `generated_diff.txt`); the regenerated figure renders identically |
| Gas fork check | `scripts/gas_fork_check.sh` | Reported run in `results/gas_fork_check/reported_2026-10-26/`; rerun in `results/gas_fork_check/rerun_2026-10-27/`. Only `bindIdentity` differs between runs (run-to-run calldata variation). |

Raw benchmark data (`bench/results/`) and gas receipts (`results/gas_receipts.csv`) are unchanged.
