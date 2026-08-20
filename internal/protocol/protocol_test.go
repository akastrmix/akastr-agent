package protocol

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
)

type protocolFixtureSet struct {
	Schema    string                 `json:"schema"`
	Protocol  string                 `json:"protocol"`
	Golden    []protocolFixtureEntry `json:"golden"`
	Malformed []protocolFixtureEntry `json:"malformed"`
}

type protocolFixtureEntry struct {
	Name      string          `json:"name"`
	Direction string          `json:"direction"`
	Message   json.RawMessage `json:"message"`
}

func TestAuthSigningTextMatchesApprovedWireFormat(t *testing.T) {
	challenge := AuthChallenge{
		AgentID:     "123e4567-e89b-42d3-a456-426614174000",
		ChallengeID: "123e4567-e89b-42d3-a456-426614174001",
		Nonce:       "BwcHBwcHBwcHBwcHBwcHBwcHBwcHBwcHBwcHBwcHBwc",
		IssuedAt:    "2026-08-13T00:00:00.000Z",
		ExpiresAt:   "2026-08-13T00:00:15.000Z",
	}
	text, err := AuthSigningText(challenge)
	if err != nil {
		t.Fatal(err)
	}
	want := strings.Join([]string{
		AuthContext, challenge.AgentID, challenge.ChallengeID, challenge.Nonce,
		challenge.IssuedAt, challenge.ExpiresAt,
	}, "\n")
	if string(text) != want {
		t.Fatalf("signing text = %q, want %q", text, want)
	}
}

func TestDecodeRejectsUnknownEnvelopeFields(t *testing.T) {
	_, err := Decode([]byte(`{"protocol":"2026-08-20.v5","message_id":"123e4567-e89b-42d3-a456-426614174000","type":"x","sent_at":"2026-08-13T00:00:00Z","body":{},"extra":true}`))
	if err == nil {
		t.Fatal("Decode accepted an unknown envelope field")
	}
}

func TestPairedProtocolFixturesValidateCloudToAgentMessages(t *testing.T) {
	data, err := os.ReadFile("testdata/agent-protocol-v5.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixtures protocolFixtureSet
	if err := json.Unmarshal(data, &fixtures); err != nil {
		t.Fatal(err)
	}
	if fixtures.Schema != "akastr-agent-protocol-fixtures.v1" || fixtures.Protocol != Version {
		t.Fatalf("unexpected fixture set %q for %q", fixtures.Schema, fixtures.Protocol)
	}
	goldenCount := 0
	for _, fixture := range fixtures.Golden {
		if fixture.Direction != "cloud_to_agent" {
			continue
		}
		goldenCount++
		if err := decodeCloudFixture(fixture.Message); err != nil {
			t.Errorf("golden fixture %q rejected: %v", fixture.Name, err)
		}
	}
	malformedCount := 0
	for _, fixture := range fixtures.Malformed {
		if fixture.Direction != "cloud_to_agent" {
			continue
		}
		malformedCount++
		if err := decodeCloudFixture(fixture.Message); err == nil {
			t.Errorf("malformed fixture %q accepted", fixture.Name)
		}
	}
	if goldenCount == 0 || malformedCount == 0 {
		t.Fatal("paired fixtures do not cover Cloud-to-Agent messages")
	}
}

func decodeCloudFixture(data []byte) error {
	envelope, err := Decode(data)
	if err != nil {
		return err
	}
	switch envelope.Type {
	case "auth.challenge":
		body, err := DecodeBody[AuthChallenge](
			envelope, "challenge_id", "agent_id", "nonce", "issued_at", "expires_at",
		)
		if err != nil {
			return err
		}
		_, err = AuthSigningText(body)
		return err
	case "auth.accepted", "hello.accepted":
		body, err := DecodeBody[AgentIDBody](envelope, "agent_id")
		if err != nil || !ValidUUID(body.AgentID) {
			return errors.New("invalid Agent acknowledgement")
		}
		return nil
	case "operation.offer":
		_, err := DecodeOperationOffer(envelope)
		return err
	case "operation.accepted_ack":
		body, err := DecodeBody[AcceptedAckBody](envelope, "command_id", "accepted")
		if err != nil || !ValidUUID(body.CommandID) {
			return errors.New("invalid accepted acknowledgement")
		}
		return nil
	case "operation.result_ack":
		body, err := DecodeBody[ResultAckBody](envelope, "command_id", "persisted")
		if err != nil || !ValidUUID(body.CommandID) {
			return errors.New("invalid result acknowledgement")
		}
		return nil
	case "ip.snapshot_ack":
		body, err := DecodeBody[IPSnapshotAckBody](envelope, "snapshot_id", "persisted")
		if err != nil || !ValidUUID(body.SnapshotID) {
			return errors.New("invalid snapshot acknowledgement")
		}
		return nil
	case "ip.observed_ack":
		body, err := DecodeBody[IPObservationAckBody](envelope, "observation_id", "persisted")
		if err != nil || !ValidUUID(body.ObservationID) {
			return errors.New("invalid observation acknowledgement")
		}
		return nil
	case "changeip.unchanged_ack":
		body, err := DecodeBody[ChangeIPUnchangedAckBody](envelope, "command_id", "persisted")
		if err != nil || !ValidUUID(body.CommandID) {
			return errors.New("invalid ChangeIP acknowledgement")
		}
		return nil
	default:
		return fmt.Errorf("unsupported Cloud message type %q", envelope.Type)
	}
}
