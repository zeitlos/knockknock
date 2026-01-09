package config

type Config struct {
	BinaryName  string
	BinaryDir   string
	VersionsDir string
	Repo        string
	Version     string

	Auth *AuthConfig
}

type AuthConfig struct {
	Username string
	Password string
	Token    string
}

// New creates a new Config with the given binary name.
// BinaryDir defaults to "/usr/local/bin", VersionsDir defaults to "/usr/local/lib".
func New(binaryName string) *Config {
	return &Config{
		BinaryName:  binaryName,
		BinaryDir:   "/usr/local/bin",
		VersionsDir: "/usr/local/lib",
	}
}

// WithRepo sets the OCI registry repository to pull updates from
// (e.g., "ghcr.io/org/repo").
func (c *Config) WithRepo(repo string) *Config {
	c.Repo = repo
	return c
}

// WithVersion sets the current version of the binary.
// This should typically be set at build time via ldflags.
func (c *Config) WithVersion(version string) *Config {
	c.Version = version
	return c
}

// WithAuth sets the authentication credentials for the OCI registry.
func (c *Config) WithAuth(auth *AuthConfig) *Config {
	c.Auth = auth
	return c
}

// WithBinaryDir sets the directory where the executable symlink will be placed
// Default: "/usr/local/bin"
func (c *Config) WithBinaryDir(dir string) *Config {
	c.BinaryDir = dir
	return c
}

// WithVersionsDir sets the base directory for storing version data
// Default: "/usr/local/lib"
func (c *Config) WithVersionsDir(dir string) *Config {
	c.VersionsDir = dir
	return c
}
