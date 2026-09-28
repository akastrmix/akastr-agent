// Package bootstrap fetches the sealed node configuration at install time. The
// machine token authenticates the request and, with the node UUID, decrypts it.
package bootstrap

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"
)

const maxBootstrapBytes = 128 * 1024

type fetchRequest struct {
	AgentID      string `json:"agent_id"`
	MachineToken string `json:"machine_token"`
}

type fetchResponse struct {
	Schema     string `json:"schema"`
	Nonce      string `json:"nonce"`
	Ciphertext string `json:"ciphertext"`
}

// DecodeToken validates the canonical base64url machine token.
func DecodeToken(token string) ([]byte, error) {
	decoded, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil || len(decoded) != 32 || base64.RawURLEncoding.EncodeToString(decoded) != token {
		return nil, errors.New("machine token must be canonical base64url for 32 bytes")
	}
	return decoded, nil
}

// Fetch returns the decrypted configuration document. Callers validate it.
func Fetch(ctx context.Context, client *http.Client, endpoint, agentID, token string) ([]byte, error) {
	tokenBytes, err := DecodeToken(token)
	if err != nil {
		return nil, err
	}
	parsed, err := url.Parse(endpoint)
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.Path != "/internal/agents/bootstrap" ||
		parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return nil, errors.New("bootstrap endpoint must be an absolute HTTPS bootstrap URL")
	}
	body, err := json.Marshal(fetchRequest{AgentID: agentID, MachineToken: token})
	if err != nil {
		return nil, err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, parsed.String(), bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json")
	if client == nil {
		client = &http.Client{Timeout: 15 * time.Second}
	}
	strict := *client
	strict.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	response, err := strict.Do(request)
	if err != nil {
		return nil, fmt.Errorf("fetch bootstrap: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4096))
		return nil, fmt.Errorf("fetch bootstrap: server returned HTTP %d", response.StatusCode)
	}
	decoder := json.NewDecoder(io.LimitReader(response.Body, maxBootstrapBytes+1))
	decoder.DisallowUnknownFields()
	var envelope fetchResponse
	if err := decoder.Decode(&envelope); err != nil {
		return nil, fmt.Errorf("decode bootstrap response: %w", err)
	}
	if envelope.Schema != "akastr-agent-bootstrap.v4" {
		return nil, errors.New("bootstrap response schema is unsupported")
	}
	nonce, err := base64.RawURLEncoding.DecodeString(envelope.Nonce)
	if err != nil || len(nonce) != 12 {
		return nil, errors.New("bootstrap nonce is invalid")
	}
	ciphertext, err := base64.RawURLEncoding.DecodeString(envelope.Ciphertext)
	if err != nil || len(ciphertext) < 17 || len(ciphertext) > maxBootstrapBytes {
		return nil, errors.New("bootstrap ciphertext is invalid")
	}
	return decrypt(tokenBytes, agentID, nonce, ciphertext)
}

func decrypt(token []byte, agentID string, nonce, ciphertext []byte) ([]byte, error) {
	block, err := aes.NewCipher(deriveKey(token, agentID))
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	plaintext, err := aead.Open(nil, nonce, ciphertext, []byte(agentID))
	if err != nil {
		return nil, errors.New("bootstrap authentication failed")
	}
	return plaintext, nil
}

func deriveKey(token []byte, agentID string) []byte {
	extract := hmac.New(sha256.New, []byte(agentID))
	_, _ = extract.Write(token)
	prk := extract.Sum(nil)
	expand := hmac.New(sha256.New, prk)
	_, _ = expand.Write([]byte("akastr-agent-bootstrap-v3"))
	_, _ = expand.Write([]byte{1})
	return expand.Sum(nil)
}
