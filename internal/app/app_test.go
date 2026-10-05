package app

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/akastrmix/akastr-agent/internal/config"
	changefeature "github.com/akastrmix/akastr-agent/internal/modules/changeip"
	"github.com/akastrmix/akastr-agent/internal/modules/ipqualityrunner"
	"github.com/akastrmix/akastr-agent/internal/modules/xui"
	"github.com/akastrmix/akastr-agent/internal/protocol"
)

func nodeConfig(modules string) []byte {
	return []byte(`{"schema_version":5,"configuration_revision":2,
"agent_id":"123e4567-e89b-42d3-a456-426614174102","name":"HKT",
"control_endpoint":"wss://origin.example.com/internal/agents/ws","modules":` + modules + `}`)
}

func model(t *testing.T, modules string) (*Model, error) {
	t.Helper()
	cfg, err := config.Parse(nodeConfig(modules))
	if err != nil {
		return nil, err
	}
	return NewModel(cfg)
}

func TestModuleConfigurationFailsClosed(t *testing.T) {
	watch := `"ip_watch":{"interval_seconds":60,"ipv6":false}`
	for name, modules := range map[string]string{
		"no module":                 `{}`,
		"unknown module":            `{` + watch + `,"firewall":{}}`,
		"changeip without ip_watch": `{"changeip":{"provider":"command","program":"/usr/local/bin/changeip","args":[]}}`,
		"shell entry point":         `{` + watch + `,"changeip":{"provider":"command","program":"/bin/sh","args":[]}}`,
		"hidden program":            `{` + watch + `,"changeip":{"provider":"command","program":"/root/changeip","args":[]}}`,
		"unknown module field":      `{"ip_watch":{"interval_seconds":60,"ipv6":false,"extra":1}}`,
		"missing module field":      `{"ip_watch":{"interval_seconds":60}}`,
		"plain HTTP ChangeIP":       `{` + watch + `,"changeip":{"provider":"http_bearer","url":"http://x.test/","bearer_token":"t"}}`,
		"SOCKS5 without login":      `{` + watch + `,"socks5":{"port":1080}}`,
		"runner with profiles":      `{"ipquality_runner":{"profiles":[]}}`,
	} {
		if _, err := model(t, modules); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
	if _, err := config.Parse([]byte(strings.Replace(string(nodeConfig(`{`+watch+`}`)), `"schema_version":5`, `"schema_version":4`, 1))); err == nil {
		t.Error("previous schema accepted")
	}
}

type fixtureSet struct {
	Protocol  string         `json:"protocol"`
	Golden    []fixtureEntry `json:"golden"`
	Malformed []fixtureEntry `json:"malformed"`
}

type fixtureEntry struct {
	Name      string          `json:"name"`
	Direction string          `json:"direction"`
	Message   json.RawMessage `json:"message"`
}

// Cloud tests read the same file for the opposite direction.
func TestPairedProtocolFixturesValidateCloudToAgentMessages(t *testing.T) {
	data, err := os.ReadFile("../protocol/testdata/agent-protocol-v8.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixtures fixtureSet
	if err := json.Unmarshal(data, &fixtures); err != nil {
		t.Fatal(err)
	}
	if fixtures.Protocol != protocol.Version {
		t.Fatalf("fixtures describe %q", fixtures.Protocol)
	}
	checked := 0
	for _, fixture := range fixtures.Golden {
		if fixture.Direction == "cloud_to_agent" {
			checked++
			if err := decodeCloudMessage(fixture.Message); err != nil {
				t.Errorf("golden fixture %q rejected: %v", fixture.Name, err)
			}
		}
	}
	for _, fixture := range fixtures.Malformed {
		if fixture.Direction == "cloud_to_agent" {
			checked++
			if err := decodeCloudMessage(fixture.Message); err == nil {
				t.Errorf("malformed fixture %q accepted", fixture.Name)
			}
		}
	}
	if checked == 0 {
		t.Fatal("fixtures contain no Cloud-to-Agent messages")
	}
}

func decodeCloudMessage(data []byte) error {
	envelope, err := protocol.Decode(data)
	if err != nil {
		return err
	}
	switch envelope.Type {
	case "auth.challenge":
		body, err := protocol.DecodeBody[protocol.AuthChallenge](
			envelope, "challenge_id", "agent_id", "nonce", "issued_at", "expires_at")
		if err != nil {
			return err
		}
		_, err = protocol.AuthSigningText(body)
		return err
	case "auth.accepted", "hello.accepted":
		body, err := protocol.DecodeBody[protocol.AgentIDBody](envelope, "agent_id")
		if err != nil || !protocol.ValidUUID(body.AgentID) {
			return errors.New("invalid acknowledgement")
		}
		return nil
	case "operation.offer":
		offer, err := protocol.DecodeOperationOffer(envelope)
		if err != nil {
			return err
		}
		switch offer.CommandType {
		case "xui.inbounds.list", "xui.client.ensure", "xui.client.delete", "xui.client.read", "xui.client.reset_traffic":
			err = xui.New(nil, offer.CommandType).Validate(offer.Payload)
		case changefeature.CommandType:
			_, err = changefeature.DecodePayload(offer.Payload)
		case ipqualityrunner.CommandType:
			_, err = ipqualityrunner.DecodePayload(offer.Payload)
		default:
			err = fmt.Errorf("unsupported command %q", offer.CommandType)
		}
		return err
	case "operation.accepted_ack":
		body, err := protocol.DecodeBody[protocol.AcceptedAckBody](envelope, "command_id", "accepted")
		if err != nil || !protocol.ValidUUID(body.CommandID) {
			return errors.New("invalid accepted acknowledgement")
		}
		return nil
	case "report.ack":
		body, err := protocol.DecodeBody[protocol.ReportAckBody](envelope, "report_id")
		if err != nil || !protocol.ValidUUID(body.ReportID) {
			return errors.New("invalid report acknowledgement")
		}
		return nil
	default:
		return fmt.Errorf("unsupported Cloud message type %q", envelope.Type)
	}
}
