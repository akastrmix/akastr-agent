package ipwatch

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"
	"time"
)

func TestObserveFallsBackAndReturnsPublicIPv4(t *testing.T) {
	invalid := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		_, _ = response.Write([]byte("not-an-ip"))
	}))
	defer invalid.Close()
	valid := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.Header.Get("User-Agent") != "Akastr-Agent/test" {
			t.Errorf("User-Agent = %q", request.Header.Get("User-Agent"))
		}
		_, _ = response.Write([]byte("8.8.8.8\n"))
	}))
	defer valid.Close()

	now := time.Date(2026, 8, 13, 0, 0, 0, 0, time.UTC)
	observer := &Observer{
		perSourceTimeout: time.Second,
		userAgent:        "Akastr-Agent/test",
		clock:            func() time.Time { return now },
		v4Client:         invalid.Client(),
		v4Sources: []source{
			{url: invalid.URL, parse: parsePlainAddress},
			{url: valid.URL, parse: parsePlainAddress},
		},
	}
	observation, err := observer.Observe(context.Background(), IPv4)
	if err != nil {
		t.Fatalf("Observe() error = %v", err)
	}
	if observation.Address != netip.MustParseAddr("8.8.8.8") || observation.Source != valid.URL || !observation.ObservedAt.Equal(now) {
		t.Fatalf("Observe() = %#v", observation)
	}
}

func TestObserveRejectsPrivateAddress(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		_, _ = response.Write([]byte("10.0.0.1"))
	}))
	defer server.Close()
	observer := &Observer{
		perSourceTimeout: time.Second,
		userAgent:        "Akastr-Agent/test",
		clock:            time.Now,
		v4Client:         server.Client(),
		v4Sources:        []source{{url: server.URL, parse: parsePlainAddress}},
	}
	_, err := observer.Observe(context.Background(), IPv4)
	if err == nil || !strings.Contains(err.Error(), "non-public") {
		t.Fatalf("Observe() error = %v, want non-public error", err)
	}
}

func TestObserveHonorsParentCancellation(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		<-request.Context().Done()
	}))
	defer server.Close()
	observer := &Observer{
		perSourceTimeout: time.Second,
		userAgent:        "Akastr-Agent/test",
		clock:            time.Now,
		v4Client:         server.Client(),
		v4Sources:        []source{{url: server.URL, parse: parsePlainAddress}},
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := observer.Observe(ctx, IPv4)
	if err == nil {
		t.Fatal("Observe() ignored cancellation")
	}
}

func TestParseCloudflareTrace(t *testing.T) {
	trace := "fl=123\nh=example.com\nip=2001:4860:4860::8888\ncolo=HKG\n"
	if got := parseCloudflareTrace(trace); got != "2001:4860:4860::8888" {
		t.Fatalf("parseCloudflareTrace() = %q", got)
	}
}
