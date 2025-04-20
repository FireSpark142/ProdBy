package config

import (
	"bufio"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"

	"github.com/BurntSushi/toml"
)

// Config holds the application configuration
type Config struct {
	RepoPath     string       `toml:"repository_path"`
	DebounceSecs int          `toml:"debounce_seconds"`
	Backup       BackupConfig `toml:"backup"`
	// Add validation rules here if they become configurable
}

// BackupConfig holds S3/Wasabi specific settings
type BackupConfig struct {
	Bucket      string `toml:"s3_bucket"`
	EndpointURL string `toml:"s3_endpoint_url"`   // Crucial for Wasabi/S3 compatible
	Region      string `toml:"aws_region"`        // Often needed for Wasabi/S3 compatible
	Prefix      string `toml:"s3_prefix"`         // Optional folder inside bucket
	AccessKeyID string `toml:"aws_access_key_id"` // Optional: Use standard AWS creds chain if empty
	SecretKey   string `toml:"aws_secret_key"`    // Optional: Use standard AWS creds chain if empty
}

// DefaultConfigFile returns the default path for the config file
func DefaultConfigFile() (string, error) {
	configDir, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("failed to get user config dir: %w", err)
	}
	appConfigDir := filepath.Join(configDir, "git-monitor-app")
	return filepath.Join(appConfigDir, "config.toml"), nil
}

// LoadConfig loads configuration from the specified path or default path.
// Prompts for setup if the file doesn't exist.
func LoadConfig(configPath string) (*Config, error) {
	if configPath == "" {
		var err error
		configPath, err = DefaultConfigFile()
		if err != nil {
			return nil, err
		}
	}

	cfg := &Config{
		DebounceSecs: 2, // Default debounce
	}

	if _, err := os.Stat(configPath); os.IsNotExist(err) {
		log.Printf("Configuration file not found at %s. Starting initial setup.", configPath)
		err := initialSetup(configPath, cfg)
		if err != nil {
			return nil, fmt.Errorf("initial setup failed: %w", err)
		}
		log.Printf("Configuration saved to %s. Please review it.", configPath)
	} else if err != nil {
		return nil, fmt.Errorf("failed to stat config file %s: %w", configPath, err)
	}

	// Load the config file content (either existing or just created)
	if _, err := toml.DecodeFile(configPath, cfg); err != nil {
		return nil, fmt.Errorf("failed to decode config file %s: %w", configPath, err)
	}

	// Basic validation
	if cfg.RepoPath == "" {
		return nil, fmt.Errorf("repository_path must be set in the config file")
	}
	// Check if RepoPath exists and is a directory with .git inside? Maybe too strict.

	log.Printf("Configuration loaded from %s", configPath)
	return cfg, nil
}

// initialSetup guides the user through setting up the initial configuration.
func initialSetup(configPath string, cfg *Config) error {
	reader := bufio.NewReader(os.Stdin)

	fmt.Println("--- Git Monitor App Initial Setup ---")

	// Get Repository Path
	for cfg.RepoPath == "" {
		fmt.Print("Enter the full path to the Git repository you want to monitor: ")
		repoPath, _ := reader.ReadString('\n')
		repoPath = strings.TrimSpace(repoPath)
		gitDir := filepath.Join(repoPath, ".git")
		if _, err := os.Stat(gitDir); err == nil {
			cfg.RepoPath = repoPath
		} else {
			fmt.Printf("Error: '%s' does not seem to contain a .git directory. Please check the path.\n", repoPath)
		}
	}

	// Get Backup Config
	fmt.Print("Enter the S3/Wasabi bucket name: ")
	cfg.Backup.Bucket, _ = reader.ReadString('\n')
	cfg.Backup.Bucket = strings.TrimSpace(cfg.Backup.Bucket)

	fmt.Print("Enter the S3/Wasabi Endpoint URL (e.g., https://s3.us-east-1.wasabisys.com or leave blank for AWS default): ")
	cfg.Backup.EndpointURL, _ = reader.ReadString('\n')
	cfg.Backup.EndpointURL = strings.TrimSpace(cfg.Backup.EndpointURL)

	fmt.Print("Enter the AWS/Wasabi Region (e.g., us-east-1, eu-central-1): ")
	cfg.Backup.Region, _ = reader.ReadString('\n')
	cfg.Backup.Region = strings.TrimSpace(cfg.Backup.Region)

	fmt.Print("Enter an optional S3 prefix (folder) for backups (e.g., git-backups/my-repo) or leave blank: ")
	cfg.Backup.Prefix, _ = reader.ReadString('\n')
	cfg.Backup.Prefix = strings.TrimSpace(cfg.Backup.Prefix)

	fmt.Println("\n--- AWS/Wasabi Credentials ---")
	fmt.Println("It's recommended to use standard AWS credential methods (environment variables like AWS_ACCESS_KEY_ID,")
	fmt.Println("AWS_SECRET_ACCESS_KEY, or the ~/.aws/credentials file).")
	fmt.Println("You can optionally specify keys directly in the config file (less secure).")

	fmt.Print("Enter AWS/Wasabi Access Key ID (leave blank to use standard methods): ")
	cfg.Backup.AccessKeyID, _ = reader.ReadString('\n')
	cfg.Backup.AccessKeyID = strings.TrimSpace(cfg.Backup.AccessKeyID)

	fmt.Print("Enter AWS/Wasabi Secret Key (leave blank to use standard methods): ")
	cfg.Backup.SecretKey, _ = reader.ReadString('\n')
	cfg.Backup.SecretKey = strings.TrimSpace(cfg.Backup.SecretKey)

	// Ensure config directory exists
	configDir := filepath.Dir(configPath)
	if err := os.MkdirAll(configDir, 0750); err != nil { // More restrictive permissions
		return fmt.Errorf("failed to create config directory %s: %w", configDir, err)
	}

	// Save the initial config
	f, err := os.Create(configPath)
	if err != nil {
		return fmt.Errorf("failed to create config file %s: %w", configPath, err)
	}
	defer f.Close()

	// Set file permissions (more secure)
	if err := os.Chmod(configPath, 0600); err != nil {
		log.Printf("Warning: Failed to set config file permissions to 600: %v", err)
	}

	encoder := toml.NewEncoder(f)
	// Indent entries for readability
	encoder.Indent = "  "

	if err := encoder.Encode(cfg); err != nil {
		return fmt.Errorf("failed to encode config to %s: %w", configPath, err)
	}

	return nil
}
