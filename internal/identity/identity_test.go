package identity

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"os"
	"path/filepath"
	"testing"

	"github.com/akastrmix/akastr-agent/internal/state"
)

func TestLoadValidatesPersistedKeyPair(t *testing.T) {
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	filePath := filepath.Join(t.TempDir(), "identity.json")
	want := Identity{
		SchemaVersion: 1,
		AgentID:       "123e4567-e89b-42d3-a456-426614174000",
		PublicKey:     base64.RawURLEncoding.EncodeToString(publicKey),
		PrivateKey:    base64.RawURLEncoding.EncodeToString(privateKey),
	}
	if err := state.NewJSONFile(filePath).Save(want); err != nil {
		t.Fatal(err)
	}
	got, err := Load(filePath)
	if err != nil {
		t.Fatal(err)
	}
	if got.AgentID != want.AgentID {
		t.Fatalf("AgentID = %q", got.AgentID)
	}

	corrupt := want
	otherPublic, _, _ := ed25519.GenerateKey(rand.Reader)
	corrupt.PublicKey = base64.RawURLEncoding.EncodeToString(otherPublic)
	if err := state.NewJSONFile(filePath).Save(corrupt); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(filePath); err == nil {
		t.Fatal("Load accepted mismatched keys")
	}
	_ = os.Remove(filePath)
}
