// Command niclane runs per-NIC proxy lanes: each local SOCKS5/HTTP listener
// egresses through one pinned network interface, fail-closed.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/CallMeHFK/niclane/internal/bench"
	"github.com/CallMeHFK/niclane/internal/config"
	"github.com/CallMeHFK/niclane/internal/doctor"
	"github.com/CallMeHFK/niclane/internal/lane"
	"github.com/CallMeHFK/niclane/internal/metrics"
	"github.com/CallMeHFK/niclane/internal/supervisor"
	"github.com/CallMeHFK/niclane/internal/version"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "serve":
		err = cmdServe(os.Args[2:])
	case "doctor":
		err = doctor.Run(os.Stdout)
	case "test":
		err = cmdTest(os.Args[2:])
	case "bench":
		err = cmdBench(os.Args[2:])
	case "status":
		err = cmdStatus(os.Args[2:])
	case "version", "-v", "--version":
		fmt.Printf("niclane %s\n", version.Version)
	case "help", "-h", "--help":
		usage()
	default:
		usage()
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "niclane:", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `niclane — one NIC, one lane. Per-interface egress isolation for local proxies.

Usage:
  niclane serve  -c niclane.yaml     run the configured proxy lanes (SIGHUP reloads)
  niclane doctor                     inspect interfaces, capabilities, suggestions
  niclane test   -c niclane.yaml     verify each lane's real egress (exit IP)
  niclane bench  -c niclane.yaml     measure per-lane throughput against an echo sink
  niclane bench serve --listen :9999 run the echo sink on a peer host
  niclane status -c niclane.yaml     query the admin endpoint of a running instance
  niclane version                    print version

Documentation: https://github.com/CallMeHFK/niclane
`)
}

func newLogger(level string) *slog.Logger {
	var lv slog.Level
	switch strings.ToLower(level) {
	case "debug":
		lv = slog.LevelDebug
	case "warn":
		lv = slog.LevelWarn
	case "error":
		lv = slog.LevelError
	default:
		lv = slog.LevelInfo
	}
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: lv}))
}

func cmdServe(args []string) error {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	cfgPath := fs.String("c", "niclane.yaml", "path to config file")
	fs.Parse(args)

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		return err
	}
	log := newLogger(cfg.LogLevel)
	sup, err := supervisor.New(*cfgPath, log)
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := sup.Start(ctx); err != nil {
		return err
	}

	sighup := make(chan os.Signal, 1)
	signal.Notify(sighup, syscall.SIGHUP)
	defer signal.Stop(sighup)

	log.Info("niclane started", "version", version.Version, "lanes", len(cfg.Lanes),
		"reload", "send SIGHUP to apply config changes")
	for {
		select {
		case <-ctx.Done():
			log.Info("shutting down")
			sup.Stop()
			return nil
		case <-sighup:
			if err := sup.Reload(); err != nil {
				log.Warn("config reload failed", "error", err)
			} else {
				log.Info("config reloaded", "lanes", len(sup.Status().Lanes))
			}
		}
	}
}

func bindModeStr(m int64) string {
	switch m {
	case lane.BindModeDevice:
		return "device"
	case lane.BindModeSrcIP:
		return "source-ip"
	default:
		return "none"
	}
}

func cmdTest(args []string) error {
	fs := flag.NewFlagSet("test", flag.ExitOnError)
	cfgPath := fs.String("c", "niclane.yaml", "path to config file")
	only := fs.String("lane", "", "test only this lane")
	urlFlag := fs.String("url", "https://api.ipify.org", "IP echo URL used to observe the exit address")
	timeout := fs.Duration("timeout", 10*time.Second, "per-lane timeout")
	fs.Parse(args)

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		return err
	}
	if len(cfg.Lanes) == 0 {
		return errors.New("no lanes configured")
	}

	fmt.Printf("%-16s %-10s %-12s %-22s %s\n", "LANE", "HEALTHY", "BIND", "EGRESS", "RESULT")
	var failures int
	for _, lc := range cfg.Lanes {
		if *only != "" && lc.Name != *only {
			continue
		}
		reg := metrics.NewRegistry()
		l, err := lane.New(lc, newLogger("error"), reg.Register(lc.Name))
		if err != nil {
			return err
		}
		l.Refresh()
		healthy, mode, lastErr := l.Current()

		client := &http.Client{
			Timeout:   *timeout,
			Transport: &http.Transport{DialContext: l.DialContext, TLSHandshakeTimeout: 5 * time.Second},
		}
		ctx, cancel := context.WithTimeout(context.Background(), *timeout)
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, *urlFlag, nil)
		resp, err := client.Do(req)
		cancel()
		if err != nil {
			fmt.Printf("%-16s %-10v %-12s %-22s FAIL: %v\n", lc.Name, healthy, bindModeStr(mode), supervisor.EgressDesc(lc), err)
			if !healthy && lastErr != "" {
				fmt.Printf("%-16s reason: %s\n", "", lastErr)
			}
			failures++
			continue
		}
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		resp.Body.Close()
		fmt.Printf("%-16s %-10v %-12s %-22s exit-ip=%s\n", lc.Name, healthy, bindModeStr(mode), supervisor.EgressDesc(lc), strings.TrimSpace(string(body)))
	}
	if failures > 0 {
		return fmt.Errorf("%d lane(s) failed", failures)
	}
	return nil
}

func cmdBench(args []string) error {
	if len(args) > 0 && args[0] == "serve" {
		fs := flag.NewFlagSet("bench serve", flag.ExitOnError)
		listen := fs.String("listen", ":9999", "echo sink listen address")
		fs.Parse(args[1:])
		ln, err := net.Listen("tcp", *listen)
		if err != nil {
			return err
		}
		fmt.Println("bench sink listening on", ln.Addr(), "(ctrl-c to stop)")
		return bench.ServeSink(ln)
	}

	fs := flag.NewFlagSet("bench", flag.ExitOnError)
	cfgPath := fs.String("c", "niclane.yaml", "path to config file")
	lanesFlag := fs.String("lane", "", "comma-separated lane names (default: all)")
	target := fs.String("target", "", "echo sink host:port — run `niclane bench serve` on a peer host reachable through the lane")
	duration := fs.Duration("duration", 10*time.Second, "measurement duration per lane")
	block := fs.Int("block", bench.DefaultBlock, "write block size in bytes")
	fs.Parse(args)

	if *target == "" {
		return errors.New("-target is required (start a sink with: niclane bench serve --listen :9999)")
	}
	cfg, err := config.Load(*cfgPath)
	if err != nil {
		return err
	}
	want := map[string]bool{}
	if *lanesFlag != "" {
		for _, name := range strings.Split(*lanesFlag, ",") {
			want[strings.TrimSpace(name)] = true
		}
	}

	var results []bench.Result
	for _, lc := range cfg.Lanes {
		if len(want) > 0 && !want[lc.Name] {
			continue
		}
		reg := metrics.NewRegistry()
		l, err := lane.New(lc, newLogger("error"), reg.Register(lc.Name))
		if err != nil {
			return err
		}
		l.Refresh()
		results = append(results, bench.RunLane(context.Background(), l, *target, *duration, *block))
	}
	if len(results) == 0 {
		return errors.New("no lane matched")
	}

	fmt.Printf("%-16s %-24s %10s %11s  %s\n", "LANE", "TARGET", "UP MB/s", "DOWN MB/s", "RESULT")
	best := results[0]
	for _, r := range results {
		if r.Err != nil {
			fmt.Printf("%-16s %-24s %10s %11s  FAIL: %v\n", r.Lane, r.Target, "-", "-", r.Err)
			continue
		}
		fmt.Printf("%-16s %-24s %10.1f %11.1f  ok (%.1fs)\n",
			r.Lane, r.Target, r.MBytesPerSec(r.Up), r.MBytesPerSec(r.Down), r.Elapsed.Seconds())
		if r.Down > best.Down {
			best = r
		}
	}
	fmt.Printf("\nfastest lane by downstream: %s (%.1f MB/s)\n", best.Lane, best.MBytesPerSec(best.Down))
	return nil
}

func cmdStatus(args []string) error {
	fs := flag.NewFlagSet("status", flag.ExitOnError)
	cfgPath := fs.String("c", "niclane.yaml", "path to config file (used to find the admin address)")
	fs.Parse(args)

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		return err
	}
	if cfg.Admin == "" {
		return errors.New("config has no admin endpoint configured")
	}
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Get("http://" + cfg.Admin + "/status")
	if err != nil {
		return fmt.Errorf("query admin %s: %w (is niclane serve running?)", cfg.Admin, err)
	}
	defer resp.Body.Close()
	var payload supervisor.Status
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return err
	}
	fmt.Printf("niclane %s — %d lane(s)\n", payload.Version, len(payload.Lanes))
	rows := payload.Lanes
	sort.Slice(rows, func(i, j int) bool { return rows[i].Name < rows[j].Name })
	fmt.Printf("%-16s %-10s %-10s %-16s %-8s %10s %10s %8s\n",
		"LANE", "HEALTHY", "BIND", "EGRESS", "ACTIVE", "CONNS", "BYTES", "REJECTS")
	for _, r := range rows {
		fmt.Printf("%-16s %-10v %-10s %-16s %-8d %10d %10d %8d\n",
			r.Name, r.Healthy, r.BindMode, r.Egress, r.ConnsActive, r.ConnsTotal, r.BytesUp+r.BytesDown, r.Rejects)
	}
	return nil
}
