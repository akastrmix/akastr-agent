package xui

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func reply(w http.ResponseWriter, obj any) {
	_ = jsonEncode(w, map[string]any{"success": true, "obj": obj})
}

func TestSessionExpiryHTMLAndBusinessFailure(t *testing.T) {
	for _, mode := range []string{"unauthorized", "not_found", "html", "business"} {
		t.Run(mode, func(t *testing.T) {
			logins, calls := 0, 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if strings.HasSuffix(r.URL.Path, "/login") {
					logins++
					http.SetCookie(w, &http.Cookie{Name: "session", Value: "synthetic", Path: "/"})
					reply(w, nil)
					return
				}
				calls++
				if mode == "business" {
					fmt.Fprint(w, `{"success":false,"msg":"credential must never escape"}`)
					return
				}
				if _, err := r.Cookie("session"); err != nil {
					if mode == "not_found" {
						w.WriteHeader(404)
					} else if mode == "unauthorized" {
						w.WriteHeader(401)
					} else {
						fmt.Fprint(w, "<html>login</html>")
					}
					return
				}
				reply(w, []any{})
			}))
			defer server.Close()
			adapter := NewAdapter(Config{PanelURL: server.URL + "/panel-base/", Username: "synthetic", Password: "synthetic"})
			_, err := adapter.list(context.Background())
			if mode == "business" {
				if err == nil || err.Error() != "xui_business_failed" || logins != 0 {
					t.Fatalf("business failure: %v, logins %d", err, logins)
				}
			} else if err != nil || logins != 1 || calls != 2 {
				t.Fatalf("session recovery: %v logins %d calls %d", err, logins, calls)
			}
		})
	}
}
