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
	"os"
	"runtime"
	"strings"
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

type FetchOptions struct {
	Endpoint   string
	AgentID    string
	TokenFile  string
	HTTPClient *http.Client
	OutputDir  string
	IPQVersion string
	IPQSHA256  string
}

func FetchAndWrite(ctx context.Context, options FetchOptions) (Payload, error) {
	token, tokenBytes, err := readToken(options.TokenFile)
	if err != nil {
		return Payload{}, err
	}
	if !canonicalUUID.MatchString(options.AgentID) {
		return Payload{}, errors.New("bootstrap agent ID is invalid")
	}
	endpoint, err := validateBootstrapEndpoint(options.Endpoint)
	if err != nil {
		return Payload{}, err
	}
	body, err := json.Marshal(fetchRequest{AgentID: options.AgentID, MachineToken: token})
	if err != nil {
		return Payload{}, err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return Payload{}, err
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json")
	client := options.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: 15 * time.Second}
	}
	strictClient := *client
	strictClient.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	response, err := strictClient.Do(request)
	if err != nil {
		return Payload{}, fmt.Errorf("fetch bootstrap: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4096))
		return Payload{}, fmt.Errorf("fetch bootstrap: server returned HTTP %d", response.StatusCode)
	}
	decoder := json.NewDecoder(io.LimitReader(response.Body, maxBootstrapBytes+1))
	decoder.DisallowUnknownFields()
	var envelope fetchResponse
	if err := decoder.Decode(&envelope); err != nil {
		return Payload{}, fmt.Errorf("decode bootstrap response: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return Payload{}, errors.New("bootstrap response contains trailing JSON")
	}
	if envelope.Schema != "akastr-agent-bootstrap.v3" {
		return Payload{}, errors.New("bootstrap response schema is unsupported")
	}
	nonce, err := base64.RawURLEncoding.DecodeString(envelope.Nonce)
	if err != nil || len(nonce) != 12 {
		return Payload{}, errors.New("bootstrap nonce is invalid")
	}
	ciphertext, err := base64.RawURLEncoding.DecodeString(envelope.Ciphertext)
	if err != nil || len(ciphertext) < 17 || len(ciphertext) > maxBootstrapBytes {
		return Payload{}, errors.New("bootstrap ciphertext is invalid")
	}
	plaintext, err := decrypt(tokenBytes, options.AgentID, nonce, ciphertext)
	if err != nil {
		return Payload{}, err
	}
	var payload Payload
	plainDecoder := json.NewDecoder(bytes.NewReader(plaintext))
	plainDecoder.DisallowUnknownFields()
	if err := plainDecoder.Decode(&payload); err != nil {
		return Payload{}, fmt.Errorf("decode bootstrap payload: %w", err)
	}
	if err := plainDecoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return Payload{}, errors.New("bootstrap payload contains trailing JSON")
	}
	if err := payload.Validate(options.AgentID); err != nil {
		return Payload{}, err
	}
	if err := writeFiles(options.OutputDir, payload, token, options.IPQVersion, options.IPQSHA256); err != nil {
		return Payload{}, err
	}
	return payload, nil
}

func validateBootstrapEndpoint(raw string) (string, error) {
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.Path != "/internal/agents/bootstrap" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", errors.New("bootstrap endpoint must be an absolute HTTPS bootstrap URL")
	}
	return parsed.String(), nil
}

func readToken(filePath string) (string, []byte, error) {
	info, err := os.Stat(filePath)
	if err != nil {
		return "", nil, fmt.Errorf("stat bootstrap token: %w", err)
	}
	if !info.Mode().IsRegular() || (runtime.GOOS != "windows" && info.Mode().Perm()&0o077 != 0) {
		return "", nil, errors.New("bootstrap token must be a root-only regular file")
	}
	raw, err := os.ReadFile(filePath)
	if err != nil {
		return "", nil, fmt.Errorf("read bootstrap token: %w", err)
	}
	token := strings.TrimSpace(string(raw))
	decoded, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil || len(decoded) != 32 || base64.RawURLEncoding.EncodeToString(decoded) != token {
		return "", nil, errors.New("bootstrap token must be canonical base64url for 32 bytes")
	}
	return token, decoded, nil
}

func decrypt(token []byte, agentID string, nonce, ciphertext []byte) ([]byte, error) {
	key := deriveKey(token, agentID)
	block, err := aes.NewCipher(key)
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
