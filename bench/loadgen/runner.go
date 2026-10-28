package main

// Platform-independent load generation and measurement.
//
// Workload: rent payments (payRent / PayRent) on K pre-activated leases, each with its own
// landlord and tenant. Load is open-loop at a fixed offered rate R (one submission every 1/R s),
// as in Hyperledger Caliper's fixed-rate controller. A lease has at most one payment in flight
// (a tenant pays one period at a time), so each submission goes to the least recently used idle
// lease. If no lease is idle the submission is skipped and counted (skipped_no_idle_lease).
// This occurs only beyond saturation; the generator then behaves as a closed loop with K clients.
//
// Latency (Hyperledger PSWG definition): time from the moment the client starts submitting a
// transaction until the client observes it committed in a block on the node it is connected to.
// Throughput: successfully committed transactions whose commit was observed inside the
// measurement window, divided by the window length.

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"sort"
	"strconv"
	"sync"
	"time"
)

// Pending is an in-flight transaction.
type Pending interface {
	// Wait blocks until the transaction is committed (or fails / times out).
	Wait(ctx context.Context) (ok bool, committedAt time.Time, reason string)
}

// Backend abstracts a platform.
type Backend interface {
	Name() string
	NumLeases() int
	// Submit sends a rent payment for lease i and returns once the transaction has been
	// handed to the network (endorsed and sent to ordering on Fabric; accepted into the
	// node's transaction pool on Besu).
	Submit(ctx context.Context, lease int) (Pending, error)
	Close()
}

type record struct {
	Lease     int
	Submit    time.Time
	Commit    time.Time
	Status    string // ok | failed | timeout | submit_error
	Reason    string
	InWindow  bool // submitted inside the measurement window
	ObsWindow bool // commit observed inside the measurement window
}

// RunConfig describes one measurement run.
type RunConfig struct {
	Rate     float64
	Warmup   time.Duration
	Duration time.Duration
	Drain    time.Duration
	Label    string
	OutDir   string
	ProcPats []string
}

// Summary is written as JSON per run.
type Summary struct {
	Platform        string             `json:"platform"`
	Label           string             `json:"label"`
	OfferedTPS      float64            `json:"offered_tps"`
	SendTPS         float64            `json:"send_tps"`
	ThroughputTPS   float64            `json:"throughput_tps"`
	Submitted       int                `json:"submitted_in_window"`
	Succeeded       int                `json:"succeeded_in_window"`
	Failed          int                `json:"failed_in_window"`
	TimedOut        int                `json:"timed_out_in_window"`
	SubmitErrors    int                `json:"submit_errors_in_window"`
	Skipped         int                `json:"skipped_no_idle_lease"`
	LatencyMeanMs   float64            `json:"latency_mean_ms"`
	LatencyP50Ms    float64            `json:"latency_p50_ms"`
	LatencyP95Ms    float64            `json:"latency_p95_ms"`
	LatencyP99Ms    float64            `json:"latency_p99_ms"`
	LatencyMaxMs    float64            `json:"latency_max_ms"`
	WindowSec       float64            `json:"window_s"`
	CPU             map[string]float64 `json:"cpu_percent_of_host"`
	RSSMB           map[string]float64 `json:"rss_mb_mean"`
	FailureReasons  map[string]int     `json:"failure_reasons"`
	StartedAt       string             `json:"started_at"`
}

// Run executes one open-loop measurement.
func Run(ctx context.Context, b Backend, cfg RunConfig) (*Summary, error) {
	k := b.NumLeases()
	idle := make(chan int, k)
	for i := 0; i < k; i++ {
		idle <- i
	}
	var (
		mu      sync.Mutex
		records []*record
		wg      sync.WaitGroup
		skipped int
	)
	start := time.Now().Add(500 * time.Millisecond)
	winStart := start.Add(cfg.Warmup)
	winEnd := winStart.Add(cfg.Duration)
	interval := time.Duration(float64(time.Second) / cfg.Rate)

	sampler := StartProcSampler(cfg.ProcPats, winStart, winEnd)

	n := int(math.Ceil((cfg.Warmup + cfg.Duration).Seconds() * cfg.Rate))
	for i := 0; i < n; i++ {
		due := start.Add(time.Duration(i) * interval)
		if d := time.Until(due); d > 0 {
			time.Sleep(d)
		}
		var lease int
		select {
		case lease = <-idle:
		default:
			if due.After(winStart) && due.Before(winEnd) {
				skipped++
			}
			continue
		}
		rec := &record{Lease: lease, Submit: time.Now()}
		rec.InWindow = !rec.Submit.Before(winStart) && rec.Submit.Before(winEnd)
		mu.Lock()
		records = append(records, rec)
		mu.Unlock()
		wg.Add(1)
		go func(rec *record) {
			defer wg.Done()
			defer func() { idle <- rec.Lease }()
			wctx, cancel := context.WithTimeout(ctx, cfg.Drain+cfg.Duration+cfg.Warmup)
			defer cancel()
			p, err := b.Submit(wctx, rec.Lease)
			if err != nil {
				rec.Status, rec.Reason = "submit_error", shorten(err.Error())
				return
			}
			ok, at, reason := p.Wait(wctx)
			rec.Commit = at
			switch {
			case ok:
				rec.Status = "ok"
			case reason == "timeout":
				rec.Status = "timeout"
			default:
				rec.Status, rec.Reason = "failed", reason
			}
			rec.ObsWindow = !at.IsZero() && !at.Before(winStart) && at.Before(winEnd)
		}(rec)
	}
	// drain: wait for outstanding transactions up to cfg.Drain after the window
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Until(winEnd) + cfg.Drain):
	}
	usage := sampler.Stop()

	mu.Lock()
	defer mu.Unlock()
	s := &Summary{
		Platform: b.Name(), Label: cfg.Label, OfferedTPS: cfg.Rate, WindowSec: cfg.Duration.Seconds(),
		Skipped: skipped, FailureReasons: map[string]int{}, CPU: usage.CPU, RSSMB: usage.RSS,
		StartedAt: start.UTC().Format(time.RFC3339),
	}
	var lats []float64
	committedInWindow := 0
	for _, r := range records {
		if r.Status == "" { // still outstanding after drain
			r.Status = "timeout"
		}
		if r.Status == "ok" && r.ObsWindow {
			committedInWindow++
		}
		if !r.InWindow {
			continue
		}
		s.Submitted++
		switch r.Status {
		case "ok":
			s.Succeeded++
			lats = append(lats, float64(r.Commit.Sub(r.Submit).Microseconds())/1000)
		case "failed":
			s.Failed++
			s.FailureReasons[r.Reason]++
		case "timeout":
			s.TimedOut++
		case "submit_error":
			s.SubmitErrors++
			s.FailureReasons[r.Reason]++
		}
	}
	s.SendTPS = float64(s.Submitted) / cfg.Duration.Seconds()
	s.ThroughputTPS = float64(committedInWindow) / cfg.Duration.Seconds()
	if len(lats) > 0 {
		sort.Float64s(lats)
		sum := 0.0
		for _, v := range lats {
			sum += v
		}
		s.LatencyMeanMs = sum / float64(len(lats))
		s.LatencyP50Ms = pct(lats, 50)
		s.LatencyP95Ms = pct(lats, 95)
		s.LatencyP99Ms = pct(lats, 99)
		s.LatencyMaxMs = lats[len(lats)-1]
	}
	if err := writeRun(cfg, b.Name(), s, records); err != nil {
		return s, err
	}
	return s, nil
}

func pct(sorted []float64, p float64) float64 {
	if len(sorted) == 0 {
		return 0
	}
	idx := int(math.Ceil(p/100*float64(len(sorted)))) - 1
	if idx < 0 {
		idx = 0
	}
	return sorted[idx]
}

func shorten(s string) string {
	if len(s) > 160 {
		return s[:160]
	}
	return s
}

func writeRun(cfg RunConfig, platform string, s *Summary, recs []*record) error {
	if err := os.MkdirAll(cfg.OutDir, 0o755); err != nil {
		return err
	}
	base := fmt.Sprintf("%s/%s_%s", cfg.OutDir, platform, cfg.Label)
	f, err := os.Create(base + "_tx.csv")
	if err != nil {
		return err
	}
	defer f.Close()
	w := csv.NewWriter(f)
	_ = w.Write([]string{"lease", "submit_unix_ns", "commit_unix_ns", "latency_ms", "status", "in_window", "commit_in_window", "reason"})
	for _, r := range recs {
		lat := ""
		cm := ""
		if !r.Commit.IsZero() {
			cm = strconv.FormatInt(r.Commit.UnixNano(), 10)
			lat = strconv.FormatFloat(float64(r.Commit.Sub(r.Submit).Microseconds())/1000, 'f', 3, 64)
		}
		_ = w.Write([]string{strconv.Itoa(r.Lease), strconv.FormatInt(r.Submit.UnixNano(), 10), cm, lat, r.Status,
			strconv.FormatBool(r.InWindow), strconv.FormatBool(r.ObsWindow), r.Reason})
	}
	w.Flush()
	js, _ := json.MarshalIndent(s, "", "  ")
	return os.WriteFile(base+"_summary.json", js, 0o644)
}
