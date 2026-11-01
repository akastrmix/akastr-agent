package routing

import (
	"bytes"
	"encoding/json"
	"errors"
	"slices"
)

// The module owns three parts of the template: outbounds, the routing rules
// and the api service list. The panel's own api rule stays first, the routing
// domain strategy stays as the operator set it, and balancers go: nothing
// Cloud writes uses them.
var managedKeys = []string{"outbounds", "routing", "api"}

var (
	directOutbound  = json.RawMessage(`{"tag":"direct","protocol":"freedom"}`)
	blockedOutbound = json.RawMessage(`{"tag":"blocked","protocol":"blackhole"}`)
	// The rule 3x-ui puts first when the template has none.
	defaultAPIRule = json.RawMessage(`{"type":"field","inboundTag":["api"],"outboundTag":"api"}`)
)

// managedTemplate is the template with the target in its managed parts.
func managedTemplate(template map[string]json.RawMessage, t Target, rules []json.RawMessage) (map[string]json.RawMessage, error) {
	next := make(map[string]json.RawMessage, len(template)+len(managedKeys))
	for key, value := range template {
		next[key] = value
	}

	// The fallback goes first: Xray sends what no rule matches to its first outbound.
	outbounds := []json.RawMessage{}
	for _, o := range t.Outbounds {
		if o.Tag == t.Fallback {
			outbounds = append(outbounds, outboundJSON(o))
		}
	}
	outbounds = append(outbounds, directOutbound, blockedOutbound)
	for _, o := range t.Outbounds {
		if o.Tag != t.Fallback {
			outbounds = append(outbounds, outboundJSON(o))
		}
	}
	next["outbounds"], _ = json.Marshal(outbounds)

	var routing map[string]json.RawMessage
	if err := decodeObject(template["routing"], &routing); err != nil {
		return nil, err
	}
	var current []json.RawMessage
	if raw, found := routing["rules"]; found && json.Unmarshal(raw, &current) != nil {
		return nil, errors.New("template rules are not a list")
	}
	api := defaultAPIRule
	for _, rule := range current {
		if isAPIRule(rule) {
			api = rule
			break
		}
	}
	routing["rules"], _ = json.Marshal(append([]json.RawMessage{api}, rules...))
	delete(routing, "balancers")
	next["routing"], _ = json.Marshal(routing)

	var apiSection map[string]json.RawMessage
	if err := decodeObject(template["api"], &apiSection); err != nil {
		return nil, err
	}
	var services []string
	if raw, found := apiSection["services"]; found && json.Unmarshal(raw, &services) != nil {
		return nil, errors.New("template api services are not a list")
	}
	if !slices.Contains(services, routingService) {
		services = append(services, routingService)
	}
	apiSection["services"], _ = json.Marshal(services)
	next["api"], _ = json.Marshal(apiSection)
	return next, nil
}

// decodeObject reads an optional JSON object into a map that is never nil.
func decodeObject(raw json.RawMessage, out *map[string]json.RawMessage) error {
	if len(raw) != 0 && json.Unmarshal(raw, out) != nil {
		return errors.New("template section is not an object")
	}
	if *out == nil {
		*out = map[string]json.RawMessage{}
	}
	return nil
}

// isAPIRule recognizes the panel's rule for its own api inbound as 3x-ui does.
func isAPIRule(raw json.RawMessage) bool {
	var rule struct {
		InboundTag  any    `json:"inboundTag"`
		OutboundTag string `json:"outboundTag"`
	}
	if json.Unmarshal(raw, &rule) != nil || rule.OutboundTag != "api" {
		return false
	}
	switch tags := rule.InboundTag.(type) {
	case string:
		return tags == "api"
	case []any:
		return slices.Contains(tags, any("api"))
	}
	return false
}

// apiRule is the first rule of a template managedTemplate built.
func apiRule(template map[string]json.RawMessage) json.RawMessage {
	var routing struct {
		Rules []json.RawMessage `json:"rules"`
	}
	_ = json.Unmarshal(template["routing"], &routing)
	return routing.Rules[0]
}

func outboundJSON(o Outbound) json.RawMessage {
	var config map[string]json.RawMessage
	_ = json.Unmarshal(o.Config, &config)
	config["tag"], _ = json.Marshal(o.Tag)
	raw, _ := json.Marshal(config)
	return raw
}

// testConfig is the running configuration with the managed parts of template,
// for Xray to check. Its log settings stay out: building them opens the log
// files for writing, which the Agent's read-only view of the system refuses,
// and the module never changes them.
func testConfig(config, template map[string]json.RawMessage) []byte {
	next := make(map[string]json.RawMessage, len(config))
	for key, value := range config {
		next[key] = value
	}
	for _, key := range managedKeys {
		next[key] = template[key]
	}
	delete(next, "log")
	raw, _ := json.Marshal(next)
	return raw
}

// sameManaged compares the managed parts by content, not by spelling.
func sameManaged(a, b map[string]json.RawMessage) bool {
	for _, key := range managedKeys {
		if !bytes.Equal(canonical(a[key]), canonical(b[key])) {
			return false
		}
	}
	return true
}

// canonical spells JSON one way: Go writes object keys in order.
func canonical(raw json.RawMessage) []byte {
	if len(raw) == 0 {
		return nil
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var value any
	if decoder.Decode(&value) != nil {
		return raw
	}
	out, _ := json.Marshal(value)
	return out
}

func hasService(config map[string]json.RawMessage, service string) bool {
	var api struct {
		Services []string `json:"services"`
	}
	return json.Unmarshal(config["api"], &api) == nil && slices.Contains(api.Services, service)
}

func firstOutbound(config map[string]json.RawMessage) string {
	var outbounds []struct {
		Tag string `json:"tag"`
	}
	if json.Unmarshal(config["outbounds"], &outbounds) != nil || len(outbounds) == 0 {
		return ""
	}
	return outbounds[0].Tag
}
