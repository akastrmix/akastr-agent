package autoupdate

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/akastrmix/akastr-agent/internal/identity"
	"github.com/akastrmix/akastr-agent/internal/protocol"
)

// Wait uses the stable maintenance contract, independently of the business WSS.
func (c Client) Wait(ctx context.Context, endpoint, version string, revision int64, credentials identity.Identity) (bool, error) {
	request := CheckRequest{AgentID: credentials.AgentID, AgentVersion: version,
		ConfigurationRevision: revision, Protocol: protocol.Version}
	if err := c.sign(credentials, &request.Nonce, &request.SentAt, &request.Signature, func() []byte {
		return []byte(strings.Replace(string(SigningText(request)), MaintenanceAuthContext, "akastr-agent-maintenance-wait-v1", 1))
	}); err != nil {
		return false, err
	}
	var response struct {
		Check *bool `json:"check"`
	}
	if err := c.post(ctx, endpoint, "/internal/agents/maintenance-wait", version, request, &response); err != nil {
		return false, err
	}
	if response.Check == nil {
		return false, errors.New("maintenance wait response is invalid")
	}
	return *response.Check, nil
}

// Reconnection checks the latest desired state; wake-ups need no persistent queue.
func (c Client) Watch(ctx context.Context, endpoint, version string, revision int64, credentials identity.Identity, triggers chan<- struct{}) {
	connected := false
	for ctx.Err() == nil {
		check, err := c.Wait(ctx, endpoint, version, revision, credentials)
		if err == nil {
			if check || !connected {
				select {
				case triggers <- struct{}{}:
				default:
				}
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
