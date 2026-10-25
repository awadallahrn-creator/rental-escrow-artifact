package main

// Samples CPU time and resident memory of process groups from /proc once per second during the
// measurement window. CPU is reported as a percentage of total host capacity (all cores = 100%).

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const clkTck = 100 // USER_HZ on Linux

// Usage per process group.
type Usage struct {
	CPU map[string]float64
	RSS map[string]float64
}

// ProcSampler samples process groups matched by command-line substrings ("group=pattern").
type ProcSampler struct {
	groups   map[string]string
	stop     chan struct{}
	done     chan Usage
}

// StartProcSampler starts sampling; patterns are "name=substring" entries.
func StartProcSampler(patterns []string, winStart, winEnd time.Time) *ProcSampler {
	ps := &ProcSampler{groups: map[string]string{}, stop: make(chan struct{}), done: make(chan Usage, 1)}
	for _, p := range patterns {
		if k, v, ok := strings.Cut(p, "="); ok {
			ps.groups[k] = v
		}
	}
	ps.groups["loadgen"] = "self"
	go ps.loop(winStart, winEnd)
	return ps
}

// Stop ends sampling and returns averaged usage.
func (ps *ProcSampler) Stop() Usage {
	close(ps.stop)
	return <-ps.done
}

func (ps *ProcSampler) loop(winStart, winEnd time.Time) {
	ncpu := float64(numCPU())
	time.Sleep(time.Until(winStart))
	t0 := time.Now()
	ticks0 := ps.ticks()
	rssSum := map[string]float64{}
	samples := 0
	tk := time.NewTicker(time.Second)
	defer tk.Stop()
	var t1 time.Time
	var ticks1 map[string]float64
loop:
	for {
		select {
		case <-ps.stop:
			break loop
		case now := <-tk.C:
			if now.After(winEnd) {
				break loop
			}
			for g, v := range ps.rss() {
				rssSum[g] += v
			}
			samples++
		}
	}
	t1 = time.Now()
	ticks1 = ps.ticks()
	u := Usage{CPU: map[string]float64{}, RSS: map[string]float64{}}
	el := t1.Sub(t0).Seconds()
	for g := range ps.groups {
		u.CPU[g] = (ticks1[g] - ticks0[g]) / clkTck / el / ncpu * 100
		if samples > 0 {
			u.RSS[g] = rssSum[g] / float64(samples)
		}
	}
	ps.done <- u
}

func (ps *ProcSampler) match() map[string][]string {
	out := map[string][]string{}
	self := strconv.Itoa(os.Getpid())
	dirs, _ := filepath.Glob("/proc/[0-9]*")
	for _, d := range dirs {
		pid := filepath.Base(d)
		cmd, err := os.ReadFile(d + "/cmdline")
		if err != nil {
			continue
		}
		c := strings.ReplaceAll(string(cmd), "\x00", " ")
		for g, pat := range ps.groups {
			if (pat == "self" && pid == self) || (pat != "self" && strings.Contains(c, pat) && pid != self) {
				out[g] = append(out[g], pid)
			}
		}
	}
	return out
}

func (ps *ProcSampler) ticks() map[string]float64 {
	res := map[string]float64{}
	for g, pids := range ps.match() {
		for _, pid := range pids {
			b, err := os.ReadFile("/proc/" + pid + "/stat")
			if err != nil {
				continue
			}
			s := string(b)
			fields := strings.Fields(s[strings.LastIndex(s, ")")+2:])
			ut, _ := strconv.ParseFloat(fields[11], 64) // utime (field 14)
			st, _ := strconv.ParseFloat(fields[12], 64) // stime (field 15)
			res[g] += ut + st
		}
	}
	return res
}

func (ps *ProcSampler) rss() map[string]float64 {
	res := map[string]float64{}
	for g, pids := range ps.match() {
		for _, pid := range pids {
			b, err := os.ReadFile("/proc/" + pid + "/statm")
			if err != nil {
				continue
			}
			f := strings.Fields(string(b))
			pages, _ := strconv.ParseFloat(f[1], 64)
			res[g] += pages * float64(os.Getpagesize()) / (1 << 20)
		}
	}
	return res
}

func numCPU() int {
	b, err := os.ReadFile("/proc/cpuinfo")
	if err != nil {
		return 1
	}
	return strings.Count(string(b), "processor\t:")
}
