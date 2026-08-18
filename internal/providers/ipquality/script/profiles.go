package script

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"runtime"
	"sort"
)

var stableProfileID = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)

type profileFile struct {
	SchemaVersion int                `json:"schema_version"`
	Profiles      map[string]Profile `json:"profiles"`
}

type Profile struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

func loadProfiles(filePath string) (map[string]Profile, error) {
	info, err := os.Stat(filePath)
	if err != nil {
		return nil, fmt.Errorf("stat proxy profiles: %w", err)
	}
	if !info.Mode().IsRegular() {
		return nil, errors.New("proxy profiles must be a regular file")
	}
	if runtime.GOOS != "windows" && info.Mode().Perm()&0o077 != 0 {
		return nil, errors.New("proxy profile permissions must not grant group or other access")
	}
	file, err := os.Open(filePath)
	if err != nil {
		return nil, fmt.Errorf("open proxy profiles: %w", err)
	}
	defer file.Close()
	decoder := json.NewDecoder(io.LimitReader(file, 64*1024+1))
	decoder.DisallowUnknownFields()
	var document profileFile
	if err := decoder.Decode(&document); err != nil {
		return nil, fmt.Errorf("decode proxy profiles: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return nil, errors.New("proxy profiles contain trailing JSON")
	}
	if document.SchemaVersion != 1 || len(document.Profiles) == 0 || len(document.Profiles) > 128 {
		return nil, errors.New("proxy profiles schema or count is invalid")
	}
	for id, profile := range document.Profiles {
		if !stableProfileID.MatchString(id) || profile.Username == "" || profile.Password == "" || len(profile.Username) > 255 || len(profile.Password) > 255 {
			return nil, fmt.Errorf("proxy profile %q is invalid", id)
		}
	}
	return document.Profiles, nil
}

func ProfileIDs(filePath string) ([]string, error) {
	profiles, err := loadProfiles(filePath)
	if err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(profiles))
	for id := range profiles {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids, nil
}
