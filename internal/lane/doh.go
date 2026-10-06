package lane

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// dohClient resolves domains via JSON-API DNS-over-HTTPS endpoints (the
// application/dns-json shape shared by Cloudflare and AliDNS). Its HTTP
// transport dials through the lane, so the DNS query itself egresses through
// the pinned NIC.
type dohClient struct {
	urls []string
	hc   *http.Client
}

func newDoHClient(urls []string, dial func(ctx context.Context, network, address string) (net.Conn, error)) *dohClient {
	return &dohClient{
		urls: urls,
		hc: &http.Client{
			Timeout: 5 * time.Second,
			Transport: &http.Transport{
				DialContext:         dial,
				TLSHandshakeTimeout: 5 * time.Second,
				ForceAttemptHTTP2:   true,
			},
		},
	}
}

// dohResponse is the JSON shape of the DNS-JSON APIs.
type dohResponse struct {
	Status int `json:"Status"`
	Answer []struct {
		Name string `json:"name"`
		Type int    `json:"type"`
		Data string `json:"data"`
	} `json:"Answer"`
}

const (
	dohTypeA    = 1
	dohTypeAAAA = 28
)

// Resolve queries the configured endpoints in order (A then AAAA each) until
// answers arrive. IP literals pass through untouched.
func (d *dohClient) Resolve(ctx context.Context, host string) ([]net.IP, error) {
	if ip := net.ParseIP(host); ip != nil {
		return []net.IP{ip}, nil
	}
	var ips []net.IP
	var lastErr error
	for _, base := range d.urls {
		for _, qtype := range []int{dohTypeA, dohTypeAAAA} {
			part, err := d.query(ctx, base, host, qtype)
			if err != nil {
				lastErr = err
				continue
			}
			ips = append(ips, part...)
		}
		if len(ips) > 0 {
			return ips, nil
		}
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("no answer from any doh endpoint")
	}
	return nil, fmt.Errorf("doh resolve %q: %w", host, lastErr)
}

func (d *dohClient) query(ctx context.Context, base, host string, qtype int) ([]net.IP, error) {
	sep := "?"
	if strings.Contains(base, "?") {
		sep = "&"
	}
	endpoint := fmt.Sprintf("%s%sname=%s&type=%d", base, sep, url.QueryEscape(host), qtype)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/dns-json")
	resp, err := d.hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("http %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	if err != nil {
		return nil, err
	}
	var dr dohResponse
	if err := json.Unmarshal(body, &dr); err != nil {
		return nil, fmt.Errorf("bad doh payload: %w", err)
	}
	if dr.Status != 0 {
		return nil, fmt.Errorf("dns status %d", dr.Status)
	}
	var ips []net.IP
	for _, a := range dr.Answer {
		if a.Type == dohTypeA || a.Type == dohTypeAAAA {
			if ip := net.ParseIP(a.Data); ip != nil {
				ips = append(ips, ip)
			}
		}
	}
	return ips, nil
}
