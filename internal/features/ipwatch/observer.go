package ipwatch

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"time"

	"github.com/akastrmix/akastr-agent/internal/netpolicy"
)

const maxResponseBytes = 4096

type Family int

const (
	IPv4 Family = 4
	IPv6 Family = 6
)

type Observation struct {
	Address    netip.Addr
	Source     string
	ObservedAt time.Time
}

type source struct {
	url   string
	parse func(string) string
}

type Observer struct {
	perSourceTimeout time.Duration
	userAgent        string
	clock            func() time.Time
	v4Client         *http.Client
	v6Client         *http.Client
	v4Sources        []source
	v6Sources        []source
}

func New(perSourceTimeout time.Duration, userAgent string) (*Observer, error) {
	if perSourceTimeout <= 0 || perSourceTimeout > 30*time.Second {
		return nil, errors.New("IP observation timeout must be between 1ns and 30s")
	}
	if strings.TrimSpace(userAgent) == "" {
		return nil, errors.New("IP observation user agent is required")
	}
	return &Observer{
		perSourceTimeout: perSourceTimeout,
		userAgent:        userAgent,
		clock:            time.Now,
		v4Client:         newFamilyClient("tcp4", perSourceTimeout),
		v6Client:         newFamilyClient("tcp6", perSourceTimeout),
		v4Sources: []source{
			{url: "https://api.ipify.org", parse: parsePlainAddress},
			{url: "https://www.cloudflare.com/cdn-cgi/trace", parse: parseCloudflareTrace},
		},
		v6Sources: []source{
			{url: "https://api6.ipify.org", parse: parsePlainAddress},
			{url: "https://www.cloudflare.com/cdn-cgi/trace", parse: parseCloudflareTrace},
		},
	}, nil
}

func (o *Observer) Observe(ctx context.Context, family Family) (Observation, error) {
	client, sources, err := o.familyResources(family)
	if err != nil {
		return Observation{}, err
	}

	failures := make([]string, 0, len(sources))
	for _, candidate := range sources {
		address, err := o.fetch(ctx, client, candidate, family)
		if err == nil {
			return Observation{Address: address, Source: candidate.url, ObservedAt: o.clock().UTC()}, nil
		}
		if ctx.Err() != nil {
			return Observation{}, ctx.Err()
		}
		failures = append(failures, fmt.Sprintf("%s: %v", candidate.url, err))
	}
	return Observation{}, fmt.Errorf("observe IPv%d: %s", family, strings.Join(failures, "; "))
}

func (o *Observer) familyResources(family Family) (*http.Client, []source, error) {
	switch family {
	case IPv4:
		return o.v4Client, o.v4Sources, nil
	case IPv6:
		return o.v6Client, o.v6Sources, nil
	default:
		return nil, nil, fmt.Errorf("unsupported IP family %d", family)
	}
}

func (o *Observer) fetch(ctx context.Context, client *http.Client, candidate source, family Family) (netip.Addr, error) {
	requestContext, cancel := context.WithTimeout(ctx, o.perSourceTimeout)
	defer cancel()

	request, err := http.NewRequestWithContext(requestContext, http.MethodGet, candidate.url, nil)
	if err != nil {
		return netip.Addr{}, err
	}
	request.Header.Set("User-Agent", o.userAgent)
	request.Header.Set("Accept", "text/plain")

	response, err := client.Do(request)
	if err != nil {
		return netip.Addr{}, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return netip.Addr{}, fmt.Errorf("unexpected HTTP status %d", response.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, maxResponseBytes+1))
	if err != nil {
		return netip.Addr{}, err
	}
	if len(body) > maxResponseBytes {
		return netip.Addr{}, errors.New("response exceeds size limit")
	}

	value := candidate.parse(string(body))
	address, err := netip.ParseAddr(value)
	if err != nil {
		return netip.Addr{}, errors.New("response does not contain a valid IP address")
	}
	address = address.Unmap()
	if (family == IPv4 && !address.Is4()) || (family == IPv6 && !address.Is6()) {
		return netip.Addr{}, fmt.Errorf("response contains IPv%d instead of IPv%d", address.BitLen(), family)
	}
	if (family == IPv4 && !netpolicy.IsPublicIPv4(address)) ||
		(family == IPv6 && (!address.IsGlobalUnicast() || address.IsPrivate() || address.IsLoopback() || address.IsLinkLocalUnicast())) {
		return netip.Addr{}, errors.New("response contains a non-public IP address")
	}
	return address, nil
}

func newFamilyClient(network string, timeout time.Duration) *http.Client {
	dialer := &net.Dialer{Timeout: timeout, KeepAlive: 30 * time.Second}
	transport := &http.Transport{
		Proxy: nil,
		DialContext: func(ctx context.Context, _ string, address string) (net.Conn, error) {
			return dialer.DialContext(ctx, network, address)
		},
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          4,
		MaxIdleConnsPerHost:   1,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   timeout,
		ResponseHeaderTimeout: timeout,
	}
	return &http.Client{
		Transport: transport,
		Timeout:   timeout,
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			return errors.New("IP observation endpoint redirected")
		},
	}
}

func parsePlainAddress(body string) string {
	return strings.TrimSpace(body)
}

func parseCloudflareTrace(body string) string {
	for _, line := range strings.Split(body, "\n") {
		if value, found := strings.CutPrefix(strings.TrimSpace(line), "ip="); found {
			return strings.TrimSpace(value)
		}
	}
	return ""
}
