package cloud

import (
	"encoding/json"
	"os"
	"path/filepath"
)

const CloudURL = "https://yu-cloud.matao-xjtu.workers.dev"

// MachineConfig is stored in ~/.config/yu/cloud.json after pairing.
type MachineConfig struct {
	MachineID string `json:"machine_id"`
	Secret    string `json:"secret"`
	Name      string `json:"name"`
}

func ConfigPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".config", "yu", "cloud.json")
}

func LoadConfig() (*MachineConfig, error) {
	data, err := os.ReadFile(ConfigPath())
	if err != nil {
		return nil, err
	}
	var cfg MachineConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, err
	}
	return &cfg, nil
}

func SaveConfig(cfg *MachineConfig) error {
	path := ConfigPath()
	os.MkdirAll(filepath.Dir(path), 0700)
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0600)
}

// IsPaired returns true if this machine has been paired.
func IsPaired() bool {
	_, err := LoadConfig()
	return err == nil
}
