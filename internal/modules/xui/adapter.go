package xui

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// All panel HTTP, including login, subscription reads and Xray restarts, lives here.
// Error text is deliberately fixed: response bodies and URLs can contain credentials.
type Adapter struct {
	cfg    Config
	client *http.Client
	mu     sync.Mutex
}
type panelReply struct {
	Success bool            `json:"success"`
	Obj     json.RawMessage `json:"obj"`
}
type inbound struct {
	ID             int    `json:"id"`
	Remark         string `json:"remark"`
	Protocol       string `json:"protocol"`
	Port           int    `json:"port"`
	Enable         bool   `json:"enable"`
	Settings       string `json:"settings"`
	StreamSettings string `json:"streamSettings"`
}
type settings struct {
	Clients []map[string]any `json:"clients"`
	Method  string           `json:"method"`
}

func NewAdapter(cfg Config) *Adapter {
	jar, _ := cookiejar.New(nil)
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	// ParseConfig admits only literal loopback addresses; redirects are prohibited.
	transport.TLSClientConfig = &tls.Config{InsecureSkipVerify: true} // local panel certificates may be self-signed
	return &Adapter{cfg: cfg, client: &http.Client{Transport: transport, Jar: jar, Timeout: 20 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
}

func (a *Adapter) call(ctx context.Context, path string, body any, out any) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	for attempt := 0; attempt < 2; attempt++ {
		raw, expired, err := a.panelRequest(ctx, path, body)
		if expired && attempt == 0 {
			_, _, err = a.panelRequest(ctx, "login", map[string]string{"username": a.cfg.Username, "password": a.cfg.Password})
			if err != nil {
				return errors.New("xui_login_failed")
			}
			continue
		}
		if expired {
			return errors.New("xui_login_failed")
		}
		if err != nil {
			return err
		}
		if out != nil && json.Unmarshal(raw, out) != nil {
			return errors.New("xui_response_invalid")
		}
		return nil
	}
	return errors.New("xui_login_failed")
}

func (a *Adapter) panelRequest(ctx context.Context, path string, body any) (json.RawMessage, bool, error) {
	var data []byte
	method := "GET"
	if body != nil {
		data, _ = json.Marshal(body)
		method = "POST"
	}
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(a.cfg.PanelURL, "/")+"/"+path, bytes.NewReader(data))
	if err != nil {
		return nil, false, errors.New("xui_request_invalid")
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := a.client.Do(req)
	if err != nil {
		return nil, false, errors.New("xui_request_unknown")
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 8*1024*1024+1))
	if err != nil {
		return nil, false, errors.New("xui_request_unknown")
	}
	if resp.StatusCode == 401 || resp.StatusCode == 403 || resp.StatusCode == 404 || strings.HasPrefix(strings.TrimSpace(string(raw)), "<") {
		return nil, true, errors.New("xui_session_expired")
	}
	if resp.StatusCode != 200 {
		return nil, false, errors.New("xui_http_failed")
	}
	var reply panelReply
	if len(raw) > 8*1024*1024 || json.Unmarshal(raw, &reply) != nil {
		return nil, false, errors.New("xui_response_invalid")
	}
	if !reply.Success {
		return nil, false, errors.New("xui_business_failed")
	}
	return reply.Obj, false, nil
}

func (a *Adapter) list(ctx context.Context) ([]inbound, error) {
	var all []inbound
	err := a.call(ctx, "panel/api/inbounds/list", nil, &all)
	sort.Slice(all, func(i, j int) bool { return all[i].ID < all[j].ID })
	return all, err
}
func (a *Adapter) get(ctx context.Context, id int) (inbound, settings, error) {
	var ib inbound
	var s settings
	err := a.call(ctx, "panel/api/inbounds/get/"+strconv.Itoa(id), nil, &ib)
	if err != nil {
		return ib, s, err
	}
	if ib.ID != id || json.Unmarshal([]byte(ib.Settings), &s) != nil || s.Clients == nil {
		return ib, s, errors.New("xui_inbound_invalid")
	}
	return ib, s, nil
}
func (a *Adapter) write(ctx context.Context, path string, body any) error {
	return a.call(ctx, "panel/api/inbounds/"+path, body, nil)
}
func (a *Adapter) restart(ctx context.Context) error {
	return a.call(ctx, "panel/api/server/restartXrayService", map[string]any{}, nil)
}
func (a *Adapter) traffic(ctx context.Context, email string) (map[string]any, error) {
	var t map[string]any
	err := a.call(ctx, "panel/api/inbounds/getClientTraffics/"+url.PathEscape(email), nil, &t)
	if err == nil && t == nil {
		err = errors.New("xui_traffic_invalid")
	}
	return t, err
}

func (a *Adapter) links(ctx context.Context, p Payload) ([]string, error) {
	// A duplicated subId would leak another client's links; inspect locally, never return that client's data.
	all, err := a.list(ctx)
	if err != nil {
		return nil, err
	}
	matches := 0
	for _, ib := range all {
		var s settings
		if json.Unmarshal([]byte(ib.Settings), &s) != nil {
			return nil, errors.New("xui_inbound_invalid")
		}
		for _, c := range s.Clients {
			if c["subId"] == p.SubID {
				matches++
				if ib.ID != p.InboundID || c["email"] != p.Email {
					return nil, errors.New("xui_subscription_conflict")
				}
			}
		}
	}
	if matches != 1 {
		return nil, errors.New("xui_subscription_conflict")
	}
	req, err := http.NewRequestWithContext(ctx, "GET", strings.TrimRight(a.cfg.SubscriptionURL, "/")+"/"+p.SubID, nil)
	if err != nil {
		return nil, errors.New("xui_request_invalid")
	}
	req.Host = a.cfg.SubscriptionHost
	req.Header.Set("User-Agent", "AkastrAgent")
	req.Header.Set("Accept", "text/plain")
	resp, err := a.client.Do(req)
	if err != nil {
		return nil, errors.New("xui_links_unavailable")
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, errors.New("xui_links_unavailable")
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 16385))
	if err != nil || len(raw) > 16384 {
		return nil, errors.New("xui_links_too_large")
	}
	body := strings.TrimSpace(string(raw))
	if !strings.Contains(body, "://") {
		decoded, e := base64.StdEncoding.DecodeString(body)
		if e != nil {
			decoded, e = base64.RawStdEncoding.DecodeString(body)
		}
		if e != nil {
			return nil, errors.New("xui_links_invalid")
		}
		body = string(decoded)
	}
	links := []string{}
	for _, line := range strings.Split(strings.TrimSpace(body), "\n") {
		line = strings.TrimSpace(line)
		u, e := url.Parse(line)
		if e != nil || !map[string]bool{"vless": true, "vmess": true, "trojan": true, "ss": true, "hysteria": true, "hysteria2": true, "hy2": true}[u.Scheme] {
			return nil, errors.New("xui_links_invalid")
		}
		links = append(links, line)
	}
	encoded, _ := json.Marshal(links)
	if len(encoded) > 6000 {
		return nil, errors.New("xui_links_too_large")
	}
	return links, nil
}
