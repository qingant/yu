package cloud

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"

	qrcode "github.com/skip2/go-qrcode"
)

// Pair registers the machine with Yu Cloud and displays a QR code for device pairing.
func Pair() error {
	cfg, err := LoadConfig()
	if err != nil {
		// First time — register machine
		cfg, err = registerMachine()
		if err != nil {
			return fmt.Errorf("registering machine: %w", err)
		}
		if err := SaveConfig(cfg); err != nil {
			return fmt.Errorf("saving config: %w", err)
		}
		fmt.Printf("Machine registered: %s (%s)\n", cfg.Name, cfg.MachineID[:8])
	} else {
		fmt.Printf("Machine already registered: %s (%s)\n", cfg.Name, cfg.MachineID[:8])
	}

	// Generate pairing code
	code, err := generatePairCode(cfg)
	if err != nil {
		return fmt.Errorf("generating pair code: %w", err)
	}

	fmt.Printf("\nPairing code: %s\n", code)
	fmt.Printf("Or scan this QR code with the Yu app:\n\n")

	// QR URL — app will handle this deep link
	qrURL := fmt.Sprintf("https://yu.dev/pair?code=%s", code)
	printQR(qrURL)

	fmt.Printf("\nCode expires in 5 minutes.\n")
	return nil
}

func registerMachine() (*MachineConfig, error) {
	hostname, _ := os.Hostname()
	if hostname == "" {
		hostname = "My Mac"
	}

	body, _ := json.Marshal(map[string]string{"name": hostname})
	resp, err := http.Post(CloudURL+"/api/machines", "application/json", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		data, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("server error %d: %s", resp.StatusCode, data)
	}

	var result struct {
		ID     string `json:"id"`
		Secret string `json:"secret"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, err
	}

	return &MachineConfig{
		MachineID: result.ID,
		Secret:    result.Secret,
		Name:      hostname,
	}, nil
}

func generatePairCode(cfg *MachineConfig) (string, error) {
	body, _ := json.Marshal(map[string]string{
		"machine_id": cfg.MachineID,
		"secret":     cfg.Secret,
	})
	resp, err := http.Post(CloudURL+"/api/pair-code", "application/json", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		data, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("server error %d: %s", resp.StatusCode, data)
	}

	var result struct {
		Code string `json:"code"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", err
	}
	return result.Code, nil
}

func printQR(data string) {
	qr, err := qrcode.New(data, qrcode.Medium)
	if err != nil {
		fmt.Println(data)
		return
	}
	fmt.Println(qr.ToSmallString(false))
}
