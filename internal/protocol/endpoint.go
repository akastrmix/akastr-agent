package protocol

import "net/url"

// controlPath is where Cloud takes the Agent's control connection.
const controlPath = "/internal/agents/ws"

// ValidControlEndpoint reports whether endpoint is Cloud's control URL:
// wss://<host>/internal/agents/ws without user, query or fragment.
func ValidControlEndpoint(endpoint string) bool {
	_, ok := parseControlEndpoint(endpoint)
	return ok
}

// ControlHTTPS is the HTTPS URL of path on the host of a valid control
// endpoint.
func ControlHTTPS(endpoint, path string) (string, bool) {
	u, ok := parseControlEndpoint(endpoint)
	if !ok {
		return "", false
	}
	u.Scheme, u.Path = "https", path
	return u.String(), true
}

func parseControlEndpoint(endpoint string) (*url.URL, bool) {
	u, err := url.Parse(endpoint)
	if err != nil || u.Scheme != "wss" || u.Host == "" || u.Path != controlPath ||
		u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return nil, false
	}
	return u, true
}
