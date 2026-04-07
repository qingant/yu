package fsjail

// Profile represents a sandbox profile for filesystem isolation.
type Profile struct {
	// ProjectDir is the allowed project directory (read-write).
	ProjectDir string

	// TmpDir is the sandbox's temporary directory (read-write).
	TmpDir string

	// DenyPaths are paths the agent cannot access.
	DenyPaths []string
}

// ProfileGenerator generates platform-specific sandbox profiles.
type ProfileGenerator interface {
	// Generate creates a profile file and returns its path.
	Generate(p Profile) (string, error)

	// WrapCommand wraps a command to run inside the jail.
	// Returns the new command and args.
	WrapCommand(profilePath string, command []string) (string, []string)
}
