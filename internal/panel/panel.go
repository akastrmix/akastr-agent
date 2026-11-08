// Package panel is the client of the local 3x-ui panel that the xui and routing
// modules share: its login and the few requests they make. Cloud names none of
// them; every path and body is fixed here.
//
// Error text is deliberately fixed: response bodies and URLs can contain
// credentials.
package panel

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

const maxReply = 8 * 1024 * 1024

// Client calls one panel with one login; calls run one at a time.
type Client struct {
	url, username, password string
	client                  *http.Client
	mu                      sync.Mutex
}

// Inbound is an inbound as the panel lists it.
type Inbound struct {
	ID             int          `json:"id"`
	Remark         string       `json:"remark"`
	Tag            string       `json:"tag"`
	Protocol       string       `json:"protocol"`
	Listen         string       `json:"listen"`
	Port           int          `json:"port"`
	Enable         bool         `json:"enable"`
	Settings       string       `json:"settings"`
	StreamSettings string       `json:"streamSettings"`
	ClientStats    []ClientStat `json:"clientStats"`
}

type ClientStat struct {
	Email string `json:"email"`
	Up    int64  `json:"up"`
	Down  int64  `json:"down"`
}

// Template is the panel's Xray template, from which 3x-ui builds the running
// configuration each time it starts Xray, and the outbound test URL the panel
// saves with it.
type Template struct {
	Config          map[string]json.RawMessage
	OutboundTestURL string
}

type panelReply struct {
	Success bool            `json:"success"`
	Obj     json.RawMessage `json:"obj"`
}

// New takes a URL that the module's configuration checked: a literal loopback
// address, so redirects are refused and a self-signed certificate is fine.
func New(panelURL, username, password string) *Client {
	jar, _ := cookiejar.New(nil)
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	transport.TLSClientConfig = &tls.Config{InsecureSkipVerify: true} // local panel certificates may be self-signed
	return &Client{url: strings.TrimRight(panelURL, "/") + "/", username: username, password: password,
		client: &http.Client{Transport: transport, Jar: jar, Timeout: 20 * time.Second,
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
}

// call sends a GET when body is nil, a form when it is url.Values, else JSON,
// and logs in once more when the session has expired.
func (c *Client) call(ctx context.Context, path string, body any, out any) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	for attempt := 0; attempt < 2; attempt++ {
		raw, expired, err := c.request(ctx, path, body)
		if expired && attempt == 0 {
			_, _, err = c.request(ctx, "login", map[string]string{"username": c.username, "password": c.password})
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

func (c *Client) request(ctx context.Context, path string, body any) (json.RawMessage, bool, error) {
	method, contentType := "POST", "application/json"
	var data []byte
	switch b := body.(type) {
	case nil:
		method = "GET"
	case url.Values:
		data, contentType = []byte(b.Encode()), "application/x-www-form-urlencoded"
	default:
		data, _ = json.Marshal(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.url+path, bytes.NewReader(data))
	if err != nil {
		return nil, false, errors.New("xui_request_invalid")
	}
	req.Header.Set("Content-Type", contentType)
	// Pages outside the API answer an expired session with 401 instead of a
	// redirect to the login page only for this header.
	req.Header.Set("X-Requested-With", "XMLHttpRequest")
	resp, err := c.client.Do(req)
	if err != nil {
		return nil, false, errors.New("xui_request_unknown")
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxReply+1))
	if err != nil {
		return nil, false, errors.New("xui_request_unknown")
	}
	if resp.StatusCode == 401 || resp.StatusCode == 403 || resp.StatusCode == 404 || strings.HasPrefix(strings.TrimSpace(string(raw)), "<") {
		return nil, true, errors.New("xui_session_expired")
	}
	if resp.StatusCode != 200 {
		return nil, false, errors.New("xui_http_failed")
	}
	var r panelReply
	if len(raw) > maxReply || json.Unmarshal(raw, &r) != nil {
		return nil, false, errors.New("xui_response_invalid")
	}
	if !r.Success {
		return nil, false, errors.New("xui_business_failed")
	}
	return r.Obj, false, nil
}

// Inbounds lists the panel's inbounds by ID, with their clients' traffic.
func (c *Client) Inbounds(ctx context.Context) ([]Inbound, error) {
	var all []Inbound
	err := c.call(ctx, "panel/api/inbounds/list", nil, &all)
	sort.Slice(all, func(i, j int) bool { return all[i].ID < all[j].ID })
	return all, err
}

// clientBody is the payload the panel's client endpoints take: the inbound and
// a settings document holding just that client.
func clientBody(inboundID int, client map[string]any) map[string]any {
	raw, _ := json.Marshal(map[string]any{"clients": []map[string]any{client}})
	return map[string]any{"id": inboundID, "settings": string(raw)}
}

func (c *Client) AddClient(ctx context.Context, inboundID int, client map[string]any) error {
	return c.call(ctx, "panel/api/inbounds/addClient", clientBody(inboundID, client), nil)
}

// UpdateClient addresses the client by its identity before the update.
func (c *Client) UpdateClient(ctx context.Context, inboundID int, identity string, client map[string]any) error {
	return c.call(ctx, "panel/api/inbounds/updateClient/"+url.PathEscape(identity), clientBody(inboundID, client), nil)
}

// DeleteClient addresses the client by its email. The panel routes on the
// decoded path, so a secret holding "/" could not be addressed at all.
func (c *Client) DeleteClient(ctx context.Context, inboundID int, email string) error {
	return c.call(ctx, "panel/api/inbounds/"+strconv.Itoa(inboundID)+"/delClientByEmail/"+url.PathEscape(email), map[string]any{}, nil)
}

func (c *Client) ResetTraffic(ctx context.Context, inboundID int, email string) error {
	return c.call(ctx, "panel/api/inbounds/"+strconv.Itoa(inboundID)+"/resetClientTraffic/"+url.PathEscape(email), map[string]any{}, nil)
}

// Restart stops Xray and starts it from the template and the inbounds. It
// returns once the new process is started, which may not listen yet.
func (c *Client) Restart(ctx context.Context) error {
	return c.call(ctx, "panel/api/server/restartXrayService", map[string]any{}, nil)
}

// Template reads the Xray template. The panel sends it inside a JSON string.
func (c *Client) Template(ctx context.Context) (Template, error) {
	var text string
	if err := c.call(ctx, "panel/xray/", url.Values{}, &text); err != nil {
		return Template{}, err
	}
	var obj struct {
		XraySetting     map[string]json.RawMessage `json:"xraySetting"`
		OutboundTestURL string                     `json:"outboundTestUrl"`
	}
	if json.Unmarshal([]byte(text), &obj) != nil || obj.XraySetting == nil {
		return Template{}, errors.New("xui_response_invalid")
	}
	return Template{Config: obj.XraySetting, OutboundTestURL: obj.OutboundTestURL}, nil
}

// SaveTemplate stores the template without restarting Xray. The panel resets
// an outbound test URL left out to its default, so it is always sent back.
func (c *Client) SaveTemplate(ctx context.Context, t Template) error {
	config, err := json.Marshal(t.Config)
	if err != nil {
		return errors.New("xui_request_invalid")
	}
	return c.call(ctx, "panel/xray/update", url.Values{"xraySetting": {string(config)}, "outboundTestUrl": {t.OutboundTestURL}}, nil)
}
