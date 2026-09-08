package autoupdate

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/akastrmix/akastr-agent/internal/identity"
	"github.com/akastrmix/akastr-agent/internal/protocol"
)

// Wait uses the stable maintenance contract, independently of the business WSS.
func (c Client) Wait(ctx context.Context, endpoint, version string, revision int64, credentials identity.Identity) (bool, string, error) {
	request := CheckRequest{AgentID: credentials.AgentID, AgentVersion: version,
		ConfigurationRevision: revision, Protocol: protocol.Version}
	if err := c.sign(credentials, &request.Nonce, &request.SentAt, &request.Signature, func() []byte {
		return []byte(strings.Replace(string(SigningText(request)), MaintenanceAuthContext, "akastr-agent-maintenance-wait-v1", 1))
	}); err != nil {
		return false, "", err
	}
	var response struct {
		Check *bool `json:"check"`
	}
	var headers http.Header
	if err := c.post(ctx, endpoint, "/internal/agents/maintenance-wait", version, request, &response, &headers); err != nil {
		return false, "", err
	}
	if response.Check == nil {
		return false, "", errors.New("maintenance wait response is invalid")
	}
	retryID := headers.Get("X-Akastr-Agent-Retry")
	if len(retryID) > 64 {
		return false, "", errors.New("maintenance retry ID is invalid")
	}
	return *response.Check, retryID, nil
}

// Reconnection checks the latest desired state; wake-ups need no persistent queue.
func (c Client) Watch(ctx context.Context, endpoint, version string, revision int64, credentials identity.Identity, triggers chan Trigger) {
	connected := false
	for ctx.Err() == nil {
		check, retryID, err := c.Wait(ctx, endpoint, version, revision, credentials)
		if err == nil {
			if check || !connected {
				Notify(triggers, Trigger{RetryID: retryID})
			}
			connected = true
			continue
		}
		connected = false
		timer := time.NewTimer(10 * time.Second)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}
