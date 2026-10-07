package xui

import (
	"bytes"
	"context"
	"crypto/tls"
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

// All panel HTTP, including login and Xray restarts, lives here.
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
	ID             int          `json:"id"`
	Remark         string       `json:"remark"`
	Protocol       string       `json:"protocol"`
	Listen         string       `json:"listen"`
	Port           int          `json:"port"`
	Enable         bool         `json:"enable"`
	Settings       string       `json:"settings"`
	StreamSettings string       `json:"streamSettings"`
	ClientStats    []clientStat `json:"clientStats"`
}
type clientStat struct {
	Email string `json:"email"`
	Up    int64  `json:"up"`
	Down  int64  `json:"down"`
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

// clientBody is the payload the panel's client endpoints take: the inbound and
// a settings document holding just that client.
func clientBody(inboundID int, client map[string]any) map[string]any {
	raw, _ := json.Marshal(map[string]any{"clients": []map[string]any{client}})
	return map[string]any{"id": inboundID, "settings": string(raw)}
}
func (a *Adapter) addClient(ctx context.Context, inboundID int, client map[string]any) error {
	return a.call(ctx, "panel/api/inbounds/addClient", clientBody(inboundID, client), nil)
}

// updateClient addresses the client by its identity before the update.
func (a *Adapter) updateClient(ctx context.Context, inboundID int, identity string, client map[string]any) error {
	return a.call(ctx, "panel/api/inbounds/updateClient/"+url.PathEscape(identity), clientBody(inboundID, client), nil)
}
func (a *Adapter) deleteClient(ctx context.Context, inboundID int, identity string) error {
	return a.call(ctx, "panel/api/inbounds/"+strconv.Itoa(inboundID)+"/delClient/"+url.PathEscape(identity), map[string]any{}, nil)
}
func (a *Adapter) resetTraffic(ctx context.Context, inboundID int, email string) error {
	return a.call(ctx, "panel/api/inbounds/"+strconv.Itoa(inboundID)+"/resetClientTraffic/"+url.PathEscape(email), map[string]any{}, nil)
}
func (a *Adapter) restart(ctx context.Context) error {
	return a.call(ctx, "panel/api/server/restartXrayService", map[string]any{}, nil)
}
