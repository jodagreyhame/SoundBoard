// Package config persists user settings as JSON under
// os.UserConfigDir()/soundboard/config.json.
package config

// Settings is the persisted user configuration. Hotkeys maps a combo string
// (e.g. "ctrl+alt+1") to a clip ID.
type Settings struct {
	MicName     string            `json:"micName"`
	CableName   string            `json:"cableName"`
	MonitorName string            `json:"monitorName"`
	Monitor     bool              `json:"monitor"`
	Hotkeys     map[string]string `json:"hotkeys"`
}

// Load reads settings from disk. If the file is missing it returns a
// zero-value *Settings (not an error).
func Load() (*Settings, error) {
	panic("todo")
}

// Save creates the config directory if needed and writes pretty-printed JSON.
func (s *Settings) Save() error {
	panic("todo")
}
