package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"runtime"
	"strings"

	"github.com/spf13/cobra"
)

const (
	repoOwner = "qingant"
	repoName  = "yu"
)

type ghRelease struct {
	TagName string    `json:"tag_name"`
	Assets  []ghAsset `json:"assets"`
}

type ghAsset struct {
	Name               string `json:"name"`
	BrowserDownloadURL string `json:"browser_download_url"`
}

func updateCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "update",
		Short: "Update yu to the latest version",
		RunE: func(cmd *cobra.Command, args []string) error {
			return selfUpdate()
		},
	}
}

func selfUpdate() error {
	fmt.Printf("Current version: %s\n", version)

	// Fetch latest release from GitHub
	fmt.Print("Checking for updates... ")
	rel, err := fetchLatestRelease()
	if err != nil {
		return fmt.Errorf("checking latest release: %w", err)
	}
	fmt.Printf("latest is %s\n", rel.TagName)

	if rel.TagName == version {
		fmt.Println("Already up to date.")
		return nil
	}

	// Find the right asset for this platform
	assetName := fmt.Sprintf("yu-%s-%s", runtime.GOOS, runtime.GOARCH)
	var downloadURL string
	for _, a := range rel.Assets {
		if a.Name == assetName {
			downloadURL = a.BrowserDownloadURL
			break
		}
	}
	if downloadURL == "" {
		return fmt.Errorf("no binary found for %s/%s in release %s", runtime.GOOS, runtime.GOARCH, rel.TagName)
	}

	// Download to a temp file next to the current binary
	self, err := os.Executable()
	if err != nil {
		return fmt.Errorf("finding current binary: %w", err)
	}

	fmt.Printf("Downloading %s... ", rel.TagName)
	tmpFile := self + ".update"
	if err := downloadFile(downloadURL, tmpFile); err != nil {
		os.Remove(tmpFile)
		return fmt.Errorf("downloading: %w", err)
	}
	fmt.Println("done")

	// Make executable
	if err := os.Chmod(tmpFile, 0755); err != nil {
		os.Remove(tmpFile)
		return fmt.Errorf("chmod: %w", err)
	}

	// Atomic replace: rename new over old
	if err := os.Rename(tmpFile, self); err != nil {
		os.Remove(tmpFile)
		return fmt.Errorf("replacing binary: %w", err)
	}

	fmt.Printf("Updated to %s\n", rel.TagName)
	return nil
}

func fetchLatestRelease() (*ghRelease, error) {
	url := fmt.Sprintf("https://api.github.com/repos/%s/%s/releases/latest", repoOwner, repoName)
	resp, err := http.Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("GitHub API returned %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	var rel ghRelease
	if err := json.NewDecoder(resp.Body).Decode(&rel); err != nil {
		return nil, err
	}
	return &rel, nil
}

func downloadFile(url, dst string) error {
	resp, err := http.Get(url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return fmt.Errorf("download returned %d", resp.StatusCode)
	}

	f, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer f.Close()

	_, err = io.Copy(f, resp.Body)
	return err
}
