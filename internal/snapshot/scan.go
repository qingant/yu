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
	Name   string // relative path from project root
	SizeMB int64  // size excluding already-excluded children
}

// ScanAndPrompt scans for large directories (up to 3 levels deep) and asks
// the user about each one individually, with an option to exclude the parent instead.
// Returns new excludes to add to config.
func ScanAndPrompt(projectDir string, thresholdMB int, existingExcludes []string) []string {
	if thresholdMB <= 0 {
		return nil
	}

	excluded := buildExcludeSet(projectDir, existingExcludes)
	large := findLargeDirs(projectDir, projectDir, excluded, int64(thresholdMB), 0, 3)

	if len(large) == 0 {
		return nil
	}

	reader := bufio.NewReader(os.Stdin)
	var newExcludes []string

	fmt.Fprintf(os.Stderr, "[yu] Large directories detected:\n")
	for _, d := range large {
		fmt.Fprintf(os.Stderr, "\n  %s/ (%s)\n", d.Name, formatSize(d.SizeMB))

		parent := filepath.Dir(d.Name)
		if parent == "." {
			// Top-level dir, no parent option
			fmt.Fprintf(os.Stderr, "  Exclude from snapshots? [Y/n]: ")
		} else {
			fmt.Fprintf(os.Stderr, "  [Y] Exclude %s/  [u] Exclude parent %s/  [n] Keep: ", d.Name, parent)
		}

		input, _ := reader.ReadString('\n')
		input = strings.TrimSpace(strings.ToLower(input))

		switch input {
		case "", "y", "yes":
			newExcludes = append(newExcludes, d.Name)
			excluded[d.Name] = true
		case "u", "up":
			if parent != "." {
				newExcludes = append(newExcludes, parent)
				excluded[parent] = true
			}
		}
		// "n" or anything else → skip
	}

	return newExcludes
}

// BuildExcludeSet merges hardcoded skip list + .gitignore + config excludes.
func BuildExcludeSet(projectDir string, configExcludes []string) map[string]bool {
	return buildExcludeSet(projectDir, configExcludes)
}

func buildExcludeSet(projectDir string, configExcludes []string) map[string]bool {
	excluded := make(map[string]bool)

	for name := range skipClone {
		excluded[name] = true
	}
	for _, name := range configExcludes {
		excluded[name] = true
	}

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
// Drills down up to maxDepth to pinpoint the heavy subdirectory.
// Subtracts already-excluded children when computing parent size.
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

		fullPath := filepath.Join(dir, name)
		rel, _ := filepath.Rel(root, fullPath)

		// Skip if this path or any parent is already excluded
		if isExcluded(rel, excluded) {
			continue
		}

		// Compute size, skipping excluded subdirectories
		sizeMB := dirSizeExcluding(fullPath, root, excluded)
		if sizeMB < thresholdMB {
			continue
		}

		// Try to drill down
		if depth < maxDepth-1 {
			children := findLargeDirs(root, fullPath, excluded, thresholdMB, depth+1, maxDepth)
			if len(children) > 0 {
				result = append(result, children...)
				continue
			}
		}

		result = append(result, LargeDir{Name: rel, SizeMB: sizeMB})
	}

	return result
}

// isExcluded checks if a relative path or any of its parents is in the exclude set.
func isExcluded(rel string, excluded map[string]bool) bool {
	if excluded[rel] {
		return true
	}
	// Check parent paths: a/b/c → check a/b, then a
	for p := filepath.Dir(rel); p != "." && p != ""; p = filepath.Dir(p) {
		if excluded[p] {
			return true
		}
	}
	// Check by basename (for hardcoded skip list)
	if excluded[filepath.Base(rel)] {
		return true
	}
	return false
}

// dirSizeExcluding computes directory size, skipping excluded subdirectories.
func dirSizeExcluding(path, root string, excluded map[string]bool) int64 {
	var total int64
	filepath.Walk(path, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		if info.IsDir() && p != path {
			rel, _ := filepath.Rel(root, p)
			if isExcluded(rel, excluded) {
				return filepath.SkipDir
			}
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
