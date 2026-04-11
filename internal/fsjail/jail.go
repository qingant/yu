package fsjail

// Profile represents a sandbox profile for filesystem isolation.
type Profile struct {
	// ProjectDir is the allowed project directory (read-write).
	ProjectDir string

	// TmpDir is the sandbox's temporary directory (read-write).
	TmpDir string

	// WorkspaceDir is the yu workspace for this project (~/.yu/workspaces/<slug>/).
	WorkspaceDir string

	// AllowPaths are additional paths to allow (e.g. agent config dirs).
	AllowPaths []string

	// DenyPaths are paths the agent cannot access (credential dirs).
	DenyPaths []string

	// DenyHomeDir: if true, deny the entire home dir and re-allow specifics.
	// If false, only deny DenyPaths (less restrictive, for external agents).
	DenyHomeDir bool
}

// ProfileGenerator generates platform-specific sandbox profiles.
type ProfileGenerator interface {
	// Generate creates a profile file and returns its path.
	Generate(p Profile) (string, error)

	// WrapCommand wraps a command to run inside the jail.
	// Returns the new command and args.
	WrapCommand(profilePath string, command []string) (string, []string)
}
