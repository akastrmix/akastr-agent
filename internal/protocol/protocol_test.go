package protocol

import (
	"encoding/json"
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
	_, err := Decode([]byte(`{"protocol":"2026-08-18.v4","message_id":"123e4567-e89b-42d3-a456-426614174000","type":"x","sent_at":"2026-08-13T00:00:00Z","body":{},"extra":true}`))
	if err == nil {
		t.Fatal("Decode accepted an unknown envelope field")
	}
}

func TestPairedProtocolFixturesValidateCloudToAgentMessages(t *testing.T) {
	data, err := os.ReadFile("testdata/agent-protocol-v4.json")
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
		if _, err := DecodeServerEnvelope(fixture.Message); err != nil {
			t.Errorf("golden fixture %q rejected: %v", fixture.Name, err)
		}
	}
	malformedCount := 0
	for _, fixture := range fixtures.Malformed {
		if fixture.Direction != "cloud_to_agent" {
			continue
		}
		malformedCount++
		if _, err := DecodeServerEnvelope(fixture.Message); err == nil {
			t.Errorf("malformed fixture %q accepted", fixture.Name)
		}
	}
	if goldenCount == 0 || malformedCount == 0 {
		t.Fatal("paired fixtures do not cover Cloud-to-Agent messages")
	}
}
