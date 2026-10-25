// loadgen: identical workload, scheduler and metric definitions for Hyperledger Fabric and
// Hyperledger Besu.
//
//	loadgen setup -platform fabric|besu -leases 1000 -state state.json
//	loadgen run   -platform fabric|besu -state state.json -rate 100 -warmup 15s -duration 60s -label r100_rep1 -out results/
//	loadgen e2e   -platform fabric|besu -out results/      (functional scenarios on the live network)
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"strings"
	"time"

	ethereum "github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/common"
)

func ethCall(to common.Address, data []byte) ethereum.CallMsg {
	return ethereum.CallMsg{To: &to, Data: data}
}

func main() {
	if len(os.Args) < 2 {
		log.Fatal("usage: loadgen setup|run|e2e [flags]")
	}
	cmd := os.Args[1]
	fs := flag.NewFlagSet(cmd, flag.ExitOnError)
	platform := fs.String("platform", "", "fabric | besu")
	leases := fs.Int("leases", 1000, "number of leases (setup)")
	state := fs.String("state", "state.json", "state file")
	cryptoDir := fs.String("crypto", "/opt/bench/fabnet/crypto", "Fabric crypto material")
	artifacts := fs.String("artifacts", "../../out", "Foundry build output (out/)")
	rate := fs.Float64("rate", 50, "offered load (tx/s)")
	warmup := fs.Duration("warmup", 15*time.Second, "warm-up (excluded)")
	duration := fs.Duration("duration", 60*time.Second, "measurement window")
	drain := fs.Duration("drain", 60*time.Second, "max wait for outstanding transactions after the window")
	label := fs.String("label", "run", "run label")
	out := fs.String("out", "results", "output directory")
	procs := fs.String("procs", "", "comma-separated name=cmdline-substring groups for CPU/RSS sampling")
	_ = fs.Parse(os.Args[2:])
	ctx := context.Background()

	var b Backend
	switch *platform {
	case "fabric":
		fb, err := newFabric(*cryptoDir)
		if err != nil {
			log.Fatal(err)
		}
		defer fb.Close()
		switch cmd {
		case "setup":
			t0 := time.Now()
			if err := fb.Setup(ctx, *leases, *state); err != nil {
				log.Fatal(err)
			}
			fmt.Printf("fabric setup: %d leases active in %s\n", *leases, time.Since(t0).Round(time.Second))
			return
		case "e2e":
			if err := fabricE2E(ctx, fb, *cryptoDir, *out); err != nil {
				log.Fatal(err)
			}
			return
		}
		if err := fb.Load(*state); err != nil {
			log.Fatal(err)
		}
		b = fb
	case "besu":
		bb, err := newBesu(*artifacts)
		if err != nil {
			log.Fatal(err)
		}
		defer bb.Close()
		switch cmd {
		case "setup":
			t0 := time.Now()
			if err := bb.Setup(ctx, *leases, *state); err != nil {
				log.Fatal(err)
			}
			fmt.Printf("besu setup: %d leases active in %s\n", *leases, time.Since(t0).Round(time.Second))
			return
		case "e2e":
			if err := besuE2E(ctx, bb, *out); err != nil {
				log.Fatal(err)
			}
			return
		}
		if err := bb.Load(ctx, *state); err != nil {
			log.Fatal(err)
		}
		b = bb
	default:
		log.Fatal("-platform must be fabric or besu")
	}
	if cmd != "run" {
		log.Fatalf("unknown command %s", cmd)
	}
	var pats []string
	if *procs != "" {
		pats = strings.Split(*procs, ",")
	}
	s, err := Run(ctx, b, RunConfig{Rate: *rate, Warmup: *warmup, Duration: *duration, Drain: *drain,
		Label: *label, OutDir: *out, ProcPats: pats})
	if err != nil {
		log.Fatal(err)
	}
	js, _ := json.Marshal(s)
	fmt.Println(string(js))
}
