package script

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"time"

	xproxy "golang.org/x/net/proxy"
)

const maxOutputBytes = 2 * 1024 * 1024

var reportURLPattern = regexp.MustCompile(`(?i)https://report\.check\.place/[A-Za-z0-9._~!$&'()*+,;=:@%/?#-]+`)

var requiredCommands = []string{"/bin/bash", "bc", "curl", "dig", "ip", "jq", "nc"}

type Config struct {
	ScriptPath        string
	ProfilesFile      string
	Timeout           time.Duration
	ScriptVersion     string
	ExpectedSHA256Hex string
}

type Provider struct {
	config   Config
	profiles map[string]Profile
	now      func() time.Time
}

type Request struct {
	ProxyPort      int
	ProxyProfileID string
	ExpectedIPv4   string
}

type Result struct {
	Code       string
	ReportURL  string
	IPv4Before string
	IPv4After  string
	CheckedAt  time.Time
}

func New(config Config) (*Provider, error) {
	if config.Timeout < time.Minute || config.Timeout > 30*time.Minute {
		return nil, errors.New("IPQuality timeout must be between 1 and 30 minutes")
	}
	if config.ScriptVersion == "" {
		return nil, errors.New("IPQuality script version is required")
	}
	profiles, err := loadProfiles(config.ProfilesFile)
	if err != nil {
		return nil, err
	}
	provider := &Provider{config: config, profiles: profiles, now: time.Now}
	if err := provider.verifyScript(); err != nil {
		return nil, err
	}
	if err := verifyDependencies(); err != nil {
		return nil, err
	}
	return provider, nil
}

func verifyDependencies() error {
	for _, command := range requiredCommands {
		if _, err := exec.LookPath(command); err != nil {
			return fmt.Errorf("IPQuality required command %q is unavailable", command)
		}
	}
	return nil
}

func (p *Provider) Run(ctx context.Context, request Request) Result {
	checkedAt := p.now().UTC()
	profile, found := p.profiles[request.ProxyProfileID]
	if !found {
		return Result{Code: "proxy_profile_not_found", CheckedAt: checkedAt}
	}
	expected, err := netip.ParseAddr(request.ExpectedIPv4)
	if err != nil || !expected.Is4() || !expected.IsGlobalUnicast() || request.ProxyPort < 1 || request.ProxyPort > 65535 {
		return Result{Code: "proxy_endpoint_invalid", CheckedAt: checkedAt}
	}
	if err := p.verifyScript(); err != nil {
		return Result{Code: "script_checksum_mismatch", CheckedAt: checkedAt}
	}
	proxyAddress := net.JoinHostPort(expected.String(), fmt.Sprint(request.ProxyPort))
	before, err := observeProxyIPv4(ctx, proxyAddress, profile)
	if err != nil {
		return Result{Code: "proxy_preflight_failed", CheckedAt: checkedAt}
	}
	if before != request.ExpectedIPv4 {
		return Result{Code: "stale_expected_ipv4", IPv4Before: before, IPv4After: before, CheckedAt: checkedAt}
	}
	runContext, cancel := context.WithTimeout(ctx, p.config.Timeout)
	defer cancel()
	relay, err := startSOCKSRelay(runContext, proxyAddress, profile)
	if err != nil {
		return Result{Code: "proxy_relay_failed", IPv4Before: before, CheckedAt: checkedAt}
	}
	defer relay.Close()
	process := exec.Command("/bin/bash", p.config.ScriptPath, "-4", "-n", "-x", relay.URL())
	process.Stdin = nil
	output := &limitedBuffer{limit: maxOutputBytes}
	process.Stdout = output
	process.Stderr = io.Discard
	configureProcess(process)
	if err := process.Start(); err != nil {
		return Result{Code: "script_start_failed", IPv4Before: before, CheckedAt: checkedAt}
	}
	done := make(chan error, 1)
	go func() { done <- process.Wait() }()
	var processError error
	select {
	case processError = <-done:
	case <-runContext.Done():
		terminateProcess(process)
		<-done
		code := "script_timed_out"
		if ctx.Err() != nil {
			code = "cancelled"
		}
		return Result{Code: code, IPv4Before: before, CheckedAt: p.now().UTC()}
	}
	reportURL, outputCode := interpretScriptOutput(output, processError)
	if outputCode != "" {
		return Result{Code: outputCode, IPv4Before: before, CheckedAt: p.now().UTC()}
	}
	after, err := observeProxyIPv4(ctx, proxyAddress, profile)
	if err != nil {
		return Result{Code: "proxy_postflight_failed", ReportURL: reportURL, IPv4Before: before, CheckedAt: p.now().UTC()}
	}
	if after != before {
		return Result{Code: "proxy_ipv4_changed", ReportURL: reportURL, IPv4Before: before, IPv4After: after, CheckedAt: p.now().UTC()}
	}
	return Result{Code: "report_ready", ReportURL: reportURL, IPv4Before: before, IPv4After: after, CheckedAt: p.now().UTC()}
}

func interpretScriptOutput(output *limitedBuffer, processError error) (string, string) {
	if output.overflow {
		return "", "script_output_too_large"
	}
	reportURL := reportURLPattern.FindString(output.buffer.String())
	if reportURL != "" {
		return reportURL, ""
	}
	if processError != nil {
		return "", "script_failed"
	}
	return "", "report_url_missing"
}

func (p *Provider) verifyScript() error {
	contents, err := os.ReadFile(p.config.ScriptPath)
	if err != nil {
		return fmt.Errorf("read IPQuality script: %w", err)
	}
	expected, err := hex.DecodeString(p.config.ExpectedSHA256Hex)
	if err != nil || len(expected) != sha256.Size {
		return errors.New("invalid configured IPQuality script checksum")
	}
	actual := sha256.Sum256(contents)
	if subtle.ConstantTimeCompare(actual[:], expected) != 1 {
		return errors.New("IPQuality script checksum mismatch")
	}
	return nil
}

func observeProxyIPv4(ctx context.Context, address string, profile Profile) (string, error) {
	base := &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}
	dialer, err := xproxy.SOCKS5("tcp", address, &xproxy.Auth{User: profile.Username, Password: profile.Password}, base)
	if err != nil {
		return "", err
	}
	contextDialer, ok := dialer.(xproxy.ContextDialer)
	if !ok {
		return "", errors.New("SOCKS5 dialer does not support context")
	}
	transport := &http.Transport{
		Proxy:                 nil,
		DialContext:           contextDialer.DialContext,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: 10 * time.Second,
		IdleConnTimeout:       10 * time.Second,
	}
	defer transport.CloseIdleConnections()
	client := &http.Client{
		Transport: transport, Timeout: 15 * time.Second,
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return errors.New("redirect rejected") },
	}
	request, _ := http.NewRequestWithContext(ctx, http.MethodGet, "https://api.ipify.org", nil)
	request.Header.Set("User-Agent", "Akastr-Agent/IPQuality")
	response, err := client.Do(request)
	if err != nil {
		return "", err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return "", fmt.Errorf("unexpected HTTP status %d", response.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, 65))
	if err != nil {
		return "", err
	}
	addressValue, err := netip.ParseAddr(strings.TrimSpace(string(body)))
	if err != nil || !addressValue.Is4() || !addressValue.IsGlobalUnicast() || addressValue.IsPrivate() {
		return "", errors.New("proxy returned invalid public IPv4")
	}
	return addressValue.String(), nil
}

type limitedBuffer struct {
	buffer   bytes.Buffer
	limit    int
	overflow bool
}

func (w *limitedBuffer) Write(data []byte) (int, error) {
	remaining := w.limit - w.buffer.Len()
	if remaining <= 0 {
		w.overflow = true
		return len(data), nil
	}
	if len(data) > remaining {
		_, _ = w.buffer.Write(data[:remaining])
		w.overflow = true
		return len(data), nil
	}
	_, _ = w.buffer.Write(data)
	return len(data), nil
}
