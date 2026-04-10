package snapshot

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// LargeDir is a directory exceeding the size threshold.
type LargeDir struct {
	Name   string
	SizeMB int64
}

// ScanAndPrompt scans for large directories (up to 3 levels deep) and asks
// the user to exclude them. Drills down to find the actual heavy subdirectory
// rather than excluding a whole top-level dir.
// Returns new excludes to add to config. Returns nil if none found or user declines.
func ScanAndPrompt(projectDir string, thresholdMB int, existingExcludes []string) []string {
	if thresholdMB <= 0 {
		return nil
	}

	excluded := buildExcludeSet(projectDir, existingExcludes)
	large := findLargeDirs(projectDir, projectDir, excluded, int64(thresholdMB), 0, 3)

	if len(large) == 0 {
		return nil
	}

	fmt.Fprintf(os.Stderr, "[yu] Large directories detected:\n")
	for _, d := range large {
		fmt.Fprintf(os.Stderr, "       %-20s (%s)\n", d.Name+"/", formatSize(d.SizeMB))
	}
	fmt.Fprintf(os.Stderr, "\n     Exclude from snapshots? [Y/n]: ")

	reader := bufio.NewReader(os.Stdin)
	input, _ := reader.ReadString('\n')
	input = strings.TrimSpace(strings.ToLower(input))

	if input == "" || input == "y" || input == "yes" {
		var names []string
		for _, d := range large {
			names = append(names, d.Name)
		}
		return names
	}
	return nil
}

// BuildExcludeSet merges hardcoded skip list + .gitignore + config excludes.
// Exported so the cloner can use it.
func BuildExcludeSet(projectDir string, configExcludes []string) map[string]bool {
	return buildExcludeSet(projectDir, configExcludes)
}

func buildExcludeSet(projectDir string, configExcludes []string) map[string]bool {
	excluded := make(map[string]bool)

	// Hardcoded
	for name := range skipClone {
		excluded[name] = true
	}

	// Config
	for _, name := range configExcludes {
		excluded[name] = true
	}

	// .gitignore top-level directory patterns
	data, err := os.ReadFile(filepath.Join(projectDir, ".gitignore"))
	if err == nil {
		for _, line := range strings.Split(string(data), "\n") {
			line = strings.TrimSpace(line)
			if line == "" || strings.HasPrefix(line, "#") {
				continue
			}
			name := strings.TrimSuffix(line, "/")
			if !strings.ContainsAny(name, "/*") {
				excluded[name] = true
			}
		}
	}

	return excluded
}

// findLargeDirs recursively scans for directories exceeding thresholdMB.
// It drills down up to maxDepth levels to pinpoint the actual large subdirectory.
// e.g. if zero/ is 1GB but zero/repos/ is 900MB, it reports zero/repos/ not zero/.
func findLargeDirs(root, dir string, excluded map[string]bool, thresholdMB int64, depth, maxDepth int) []LargeDir {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}

	var result []LargeDir
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		name := entry.Name()
		if strings.HasPrefix(name, ".") || skipClone[name] {
			continue
		}

		rel, _ := filepath.Rel(root, filepath.Join(dir, name))
		if excluded[rel] || excluded[name] {
			continue
		}

		fullPath := filepath.Join(dir, name)
		sizeMB := dirSizeMB(fullPath)
		if sizeMB < thresholdMB {
			continue
		}

		// Try to drill down — find which child is responsible
		if depth < maxDepth-1 {
			children := findLargeDirs(root, fullPath, excluded, thresholdMB, depth+1, maxDepth)
			if len(children) > 0 {
				// Children account for the size, report them instead
				result = append(result, children...)
				continue
			}
		}

		// This dir itself is the culprit
		result = append(result, LargeDir{Name: rel, SizeMB: sizeMB})
	}

	return result
}

func dirSizeMB(path string) int64 {
	var total int64
	filepath.Walk(path, func(_ string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		if !info.IsDir() {
			total += info.Size()
		}
		if total > 10*1024*1024*1024 {
			return filepath.SkipAll
		}
		return nil
	})
	return total / (1024 * 1024)
}

func formatSize(mb int64) string {
	if mb >= 1024 {
		return fmt.Sprintf("%.1f GB", float64(mb)/1024)
	}
	return fmt.Sprintf("%d MB", mb)
}
