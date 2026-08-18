package protocol

import (
	"strings"
	"testing"
)

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
