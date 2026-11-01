package routing

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/netip"
	"os"
	"os/exec"
	"strconv"

	"github.com/akastrmix/akastr-agent/internal/xray/process"
)

// errInvalid is a configuration Xray refuses to build.
var errInvalid = errors.New("xray refused the configuration")

// xrayRun is one run of the Xray 3x-ui started: how to run the same binary
// again and the configuration it started with.
type xrayRun struct {
	pid    string
	binary string
	cwd    string
	env    []string
	// api is the loopback address of its api inbound.
	api    string
	config map[string]json.RawMessage
}

// xray is what the module does with the running Xray. Command output can hold
// outbound passwords: it is parsed here and never logged or reported.
type xray interface {
	locate() (xrayRun, error)
	// test builds config the way Xray would start it, without starting it.
	test(ctx context.Context, p xrayRun, config []byte) error
	outbounds(ctx context.Context, p xrayRun) ([]string, error)
	addOutbounds(ctx context.Context, p xrayRun, outbounds []json.RawMessage) error
	removeOutbounds(ctx context.Context, p xrayRun, tags []string) error
	replaceRules(ctx context.Context, p xrayRun, rules []json.RawMessage) error
	// rules says whether the api answers, by listing the rules.
	rules(ctx context.Context, p xrayRun) error
}

// cli runs the binary of the running Xray, so its commands always match it.
type cli struct{}

func (cli) locate() (xrayRun, error) {
	x, err := process.Find("/proc")
	if err != nil {
		return xrayRun{}, err
	}
	binary, err := x.Binary()
	if err != nil {
		return xrayRun{}, err
	}
	path, err := x.Config()
	if err != nil {
		return xrayRun{}, err
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return xrayRun{}, err
	}
	var config map[string]json.RawMessage
	if err := json.Unmarshal(raw, &config); err != nil {
		return xrayRun{}, err
	}
	env, err := x.AssetEnv()
	if err != nil {
		return xrayRun{}, err
	}
	api, err := apiAddress(config)
	if err != nil {
		return xrayRun{}, err
	}
	return xrayRun{pid: x.PID(), binary: binary, cwd: x.Cwd, env: env, api: api, config: config}, nil
}

// apiAddress finds the inbound 3x-ui reads Xray's statistics through, which
// must listen on a literal loopback address.
func apiAddress(config map[string]json.RawMessage) (string, error) {
	var inbounds []struct {
		Tag    string `json:"tag"`
		Listen string `json:"listen"`
		Port   int    `json:"port"`
	}
	if err := json.Unmarshal(config["inbounds"], &inbounds); err != nil {
		return "", err
	}
	for _, ib := range inbounds {
		if ib.Tag != "api" {
			continue
		}
		if ip, err := netip.ParseAddr(ib.Listen); err != nil || !ip.IsLoopback() || ib.Port < 1 || ib.Port > 65535 {
			return "", errors.New("the api inbound does not listen on loopback")
		}
		return net.JoinHostPort(ib.Listen, strconv.Itoa(ib.Port)), nil
	}
	return "", errors.New("the running Xray has no api inbound")
}

// run passes input on stdin, so configurations never touch the disk.
func (cli) run(ctx context.Context, p xrayRun, input []byte, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, p.binary, args...)
	cmd.Dir = p.cwd
	cmd.Env = append([]string{}, p.env...)
	cmd.Stdin = bytes.NewReader(input)
	return cmd.Output()
}

func (c cli) api(ctx context.Context, p xrayRun, input []byte, command string, args ...string) ([]byte, error) {
	return c.run(ctx, p, input, append([]string{"api", command, "--server=" + p.api, "-timeout=10"}, args...)...)
}

func (c cli) test(ctx context.Context, p xrayRun, config []byte) error {
	_, err := c.run(ctx, p, config, "run", "-test", "-c", "stdin:")
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		return errInvalid
	}
	return err
}

func (c cli) outbounds(ctx context.Context, p xrayRun) ([]string, error) {
	out, err := c.api(ctx, p, nil, "lso")
	if err != nil {
		return nil, err
	}
	var list struct {
		Outbounds []struct {
			Tag string `json:"tag"`
		} `json:"outbounds"`
	}
	if err := json.Unmarshal(out, &list); err != nil {
		return nil, errors.New("unreadable outbound list")
	}
	tags := make([]string, 0, len(list.Outbounds))
	for _, o := range list.Outbounds {
		tags = append(tags, o.Tag)
	}
	return tags, nil
}

func (c cli) addOutbounds(ctx context.Context, p xrayRun, outbounds []json.RawMessage) error {
	input, _ := json.Marshal(map[string]any{"outbounds": outbounds})
	_, err := c.api(ctx, p, input, "ado", "stdin:")
	return err
}

func (c cli) removeOutbounds(ctx context.Context, p xrayRun, tags []string) error {
	_, err := c.api(ctx, p, nil, "rmo", tags...)
	return err
}

// replaceRules swaps the whole table: without -append, Xray drops the rules it has.
func (c cli) replaceRules(ctx context.Context, p xrayRun, rules []json.RawMessage) error {
	input, _ := json.Marshal(map[string]any{"routing": map[string]any{"rules": rules}})
	_, err := c.api(ctx, p, input, "adrules", "stdin:")
	return err
}

func (c cli) rules(ctx context.Context, p xrayRun) error {
	_, err := c.api(ctx, p, nil, "lsrules")
	return err
}
