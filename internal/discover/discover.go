// Package discover finds proxy endpoints listening on the gateway of a
// locally attached network device, without assuming any vendor, machine or
// interface name. It never scans a subnet: only the interface's own DHCP
// gateway and the configured proxy ports are probed.
package discover

import (
	"bufio"
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/CallMeHFK/niclane/internal/config"
	"github.com/CallMeHFK/niclane/internal/lane"
	"github.com/CallMeHFK/niclane/internal/metrics"
)

// ErrUnsupported is returned on platforms without automatic discovery.
var ErrUnsupported = errors.New("automatic discovery is not supported on this platform")

// Device is one candidate endpoint found on a device gateway.
type Device struct {
	Interface string
	Gateway   net.IP
	Port      int
}

// Options controls one discovery pass.
type Options struct {
	Interfaces []string          // device | any | glob list
	Via        map[string]string // interface -> gateway override
	Ports      []int
	Timeout    time.Duration // per-port TCP probe, default 800ms
	Log        *slog.Logger
}

// Devices enumerates interfaces, resolves each one's own gateway, and
// returns the gateways with at least one open proxy port.
func Devices(ctx context.Context, o Options) ([]Device, error) {
	if !Supported() {
		return nil, ErrUnsupported
	}
	names, err := interfaceNames(o.Interfaces)
	if err != nil {
		return nil, err
	}
	timeout := o.Timeout
	if timeout <= 0 {
		timeout = 800 * time.Millisecond
	}

	type job struct {
		ifName string
		gw     net.IP
		port   int
	}
	var jobs []job
	seen := map[string]bool{}
	for _, name := range names {
		gw, gwErr := gatewayFor(ctx, name, o.Via)
		if gwErr != nil {
			if o.Log != nil {
				o.Log.Debug("discover: no gateway", "interface", name, "error", gwErr)
			}
			continue
		}
		for _, port := range o.Ports {
			key := name + "\x00" + gw.String() + "\x00" + strconv.Itoa(port)
			if seen[key] {
				continue
			}
			seen[key] = true
			jobs = append(jobs, job{ifName: name, gw: gw, port: port})
		}
	}

	sem := make(chan struct{}, 4)
	var (
		mu      sync.Mutex
		wg      sync.WaitGroup
		devices []Device
	)
	for _, j := range jobs {
		wg.Add(1)
		sem <- struct{}{}
		go func(j job) {
			defer wg.Done()
			defer func() { <-sem }()
			pctx, cancel := context.WithTimeout(ctx, timeout)
			open := portOpen(pctx, j.ifName, j.gw, j.port)
			cancel()
			if !open {
				return
			}
			mu.Lock()
			devices = append(devices, Device{Interface: j.ifName, Gateway: j.gw, Port: j.port})
			mu.Unlock()
			if o.Log != nil {
				// Per-round noise: the selector layer announces candidate
				// additions and removals, so this stays at debug level.
				o.Log.Debug("discover: proxy port open", "interface", j.ifName,
					"gateway", j.gw.String(), "port", j.port)
			}
		}(j)
	}
	wg.Wait()

	sort.Slice(devices, func(i, j int) bool {
		if devices[i].Interface != devices[j].Interface {
			return devices[i].Interface < devices[j].Interface
		}
		if a, b := devices[i].Gateway.String(), devices[j].Gateway.String(); a != b {
			return a < b
		}
		return devices[i].Port < devices[j].Port
	})
	return devices, nil
}

// portOpen dials gw:port through a throwaway lane bound to ifName, reusing
// the same binding path real candidates use.
func portOpen(ctx context.Context, ifName string, gw net.IP, port int) bool {
	lc := &config.LaneConfig{Name: "discover-probe", Interface: ifName, BindMode: config.BindModeAuto}
	l, err := lane.New(lc, slog.Default(), &metrics.Stats{})
	if err != nil {
		return false
	}
	conn, err := l.DialContext(ctx, "tcp", net.JoinHostPort(gw.String(), strconv.Itoa(port)))
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

// matchInterfaceName reports whether name is selected by spec. hasDevice tells
// whether the interface is backed by a physical device.
func matchInterfaceName(name string, hasDevice bool, spec []string) bool {
	if name == "lo" {
		return false
	}
	for _, s := range spec {
		switch s {
		case "device":
			if hasDevice {
				return true
			}
		case "any":
			return true
		default:
			if ok, _ := filepath.Match(s, name); ok {
				return true
			}
		}
	}
	return false
}

// parseProcRoute extracts the IPv4 default gateway of ifName from
// /proc/net/route. The Gateway column is little-endian hex.
func parseProcRoute(r io.Reader, ifName string) (net.IP, bool) {
	sc := bufio.NewScanner(r)
	first := true
	for sc.Scan() {
		if first {
			first = false
			continue
		}
		fields := strings.Fields(sc.Text())
		if len(fields) < 8 || fields[0] != ifName || fields[1] != "00000000" {
			continue
		}
		if ip, ok := parseRouteHex(fields[2]); ok {
			return ip, true
		}
	}
	return nil, false
}

func parseRouteHex(h string) (net.IP, bool) {
	if len(h) != 8 {
		return nil, false
	}
	v, err := strconv.ParseUint(h, 16, 32)
	if err != nil {
		return nil, false
	}
	return net.IPv4(byte(v), byte(v>>8), byte(v>>16), byte(v>>24)), true
}

// parseLease reads the ROUTER= entry of a systemd-networkd lease.
func parseLease(r io.Reader) (net.IP, bool) {
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if v, ok := strings.CutPrefix(line, "ROUTER="); ok {
			if ip := net.ParseIP(strings.TrimSpace(v)); ip != nil {
				return ip, true
			}
		}
	}
	return nil, false
}

// parseNmcliGateway reads `nmcli -t -f IP4.GATEWAY device show <if>` output.
func parseNmcliGateway(out string) (net.IP, bool) {
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if v, ok := strings.CutPrefix(line, "IP4.GATEWAY:"); ok {
			if ip := net.ParseIP(strings.TrimSpace(v)); ip != nil {
				return ip, true
			}
		}
	}
	return nil, false
}
