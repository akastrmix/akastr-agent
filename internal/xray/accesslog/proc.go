package accesslog

import (
	"encoding/json"
	"errors"
	"os"

	"github.com/akastrmix/akastr-agent/internal/xray/process"
)

// running is what the configuration of the Xray that 3x-ui runs says: where
// it writes the access log, and the tags of the outbounds that drop whatever
// is sent to them.
type running struct {
	log        string
	blackholes map[string]bool
}

// locate finds the one Xray process 3x-ui runs and reads its configuration.
// A log that is off or masks addresses cannot say who a connection belongs
// to. 3x-ui restarts Xray to change outbounds, so the file names them as they
// run.
func locate(proc string) (running, error) {
	xray, err := process.Find(proc)
	if err != nil {
		return running{}, err
	}
	config, err := xray.Config()
	if err != nil {
		return running{}, err
	}
	raw, err := os.ReadFile(config)
	if err != nil {
		return running{}, err
	}
	var doc struct {
		Log struct {
			Access      string `json:"access"`
			MaskAddress string `json:"maskAddress"`
		} `json:"log"`
		Outbounds []struct {
			Tag      string `json:"tag"`
			Protocol string `json:"protocol"`
		} `json:"outbounds"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return running{}, errors.New("the Xray configuration is not valid JSON")
	}
	if doc.Log.Access == "" || doc.Log.Access == "none" {
		return running{}, errors.New("the Xray access log is off")
	}
	if doc.Log.MaskAddress != "" {
		return running{}, errors.New("the Xray access log masks addresses")
	}
	found := running{log: xray.Resolve(doc.Log.Access), blackholes: map[string]bool{}}
	for _, o := range doc.Outbounds {
		if o.Protocol == "blackhole" && o.Tag != "" {
			found.blackholes[o.Tag] = true
		}
	}
	return found, nil
}
