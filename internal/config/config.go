package config

import (
	"fmt"
	"strings"
	"time"

	"github.com/spf13/viper"
	log "github.com/sirupsen/logrus"
)

// Config holds the application configuration
type Config struct {
	Server     ServerConfig     `mapstructure:"server"`
	Logging    LoggingConfig    `mapstructure:"logging"`
	Features   FeaturesConfig   `mapstructure:"features"`
	Cleanup    CleanupConfig    `mapstructure:"cleanup"`
	Heartbeat  HeartbeatConfig  `mapstructure:"heartbeat"`
}

// ServerConfig holds server-related configuration
type ServerConfig struct {
	Port string `mapstructure:"port"`
	Host string `mapstructure:"host"`
}

// LoggingConfig holds logging-related configuration
type LoggingConfig struct {
	Level  string `mapstructure:"level"`
	Format string `mapstructure:"format"`
}

// FeaturesConfig holds feature flags
type FeaturesConfig struct {
	CrossCodebase bool `mapstructure:"cross_codebase"`
	Persistence   bool `mapstructure:"persistence"`
}

// CleanupConfig holds cleanup-related configuration
type CleanupConfig struct {
	Enabled              bool          `mapstructure:"enabled"`
	Interval             time.Duration `mapstructure:"interval"`
	AgentOfflineTimeout  time.Duration `mapstructure:"agent_offline_timeout"`
	CompletedTaskMaxAge  time.Duration `mapstructure:"completed_task_max_age"`
}

// HeartbeatConfig holds heartbeat-related configuration
type HeartbeatConfig struct {
	Interval time.Duration `mapstructure:"interval"`
	Timeout  time.Duration `mapstructure:"timeout"`
}

// DefaultConfig returns a configuration with default values
func DefaultConfig() *Config {
	return &Config{
		Server: ServerConfig{
			Port: "8080",
			Host: "localhost",
		},
		Logging: LoggingConfig{
			Level:  "info",
			Format: "text",
		},
		Features: FeaturesConfig{
			CrossCodebase: true,
			Persistence:   false,
		},
		Cleanup: CleanupConfig{
			Enabled:              true,
			Interval:             5 * time.Minute,
			AgentOfflineTimeout:  2 * time.Minute,
			CompletedTaskMaxAge:  24 * time.Hour,
		},
		Heartbeat: HeartbeatConfig{
			Interval: 30 * time.Second,
			Timeout:  60 * time.Second,
		},
	}
}

// LoadConfig loads configuration from file, environment variables, and defaults
func LoadConfig(configFile string) (*Config, error) {
	// Set defaults
	config := DefaultConfig()

	// Configure Viper
	viper.SetConfigType("json")
	viper.SetEnvPrefix("AGENT_COORDINATOR")
	viper.SetEnvKeyReplacer(strings.NewReplacer(".", "_"))
	viper.AutomaticEnv()

	// Set default values in Viper
	viper.SetDefault("server.port", config.Server.Port)
	viper.SetDefault("server.host", config.Server.Host)
	viper.SetDefault("logging.level", config.Logging.Level)
	viper.SetDefault("logging.format", config.Logging.Format)
	viper.SetDefault("features.cross_codebase", config.Features.CrossCodebase)
	viper.SetDefault("features.persistence", config.Features.Persistence)
	viper.SetDefault("cleanup.enabled", config.Cleanup.Enabled)
	viper.SetDefault("cleanup.interval", config.Cleanup.Interval)
	viper.SetDefault("cleanup.agent_offline_timeout", config.Cleanup.AgentOfflineTimeout)
	viper.SetDefault("cleanup.completed_task_max_age", config.Cleanup.CompletedTaskMaxAge)
	viper.SetDefault("heartbeat.interval", config.Heartbeat.Interval)
	viper.SetDefault("heartbeat.timeout", config.Heartbeat.Timeout)

	// Load config file if specified
	if configFile != "" {
		viper.SetConfigFile(configFile)
		if err := viper.ReadInConfig(); err != nil {
			if _, ok := err.(viper.ConfigFileNotFoundError); !ok {
				return nil, fmt.Errorf("failed to read config file: %w", err)
			}
			// Config file not found is okay, we'll use defaults and env vars
		}
	}

	// Unmarshal configuration
	if err := viper.Unmarshal(config); err != nil {
		return nil, fmt.Errorf("failed to unmarshal config: %w", err)
	}

	// Validate configuration
	if err := config.Validate(); err != nil {
		return nil, fmt.Errorf("invalid configuration: %w", err)
	}

	return config, nil
}

// Validate validates the configuration
func (c *Config) Validate() error {
	// Validate log level
	if _, err := log.ParseLevel(c.Logging.Level); err != nil {
		return fmt.Errorf("invalid log level '%s': %w", c.Logging.Level, err)
	}

	// Validate log format
	if c.Logging.Format != "text" && c.Logging.Format != "json" {
		return fmt.Errorf("invalid log format '%s': must be 'text' or 'json'", c.Logging.Format)
	}

	// Validate server port
	if c.Server.Port == "" {
		return fmt.Errorf("server port cannot be empty")
	}

	// Validate cleanup intervals
	if c.Cleanup.Enabled {
		if c.Cleanup.Interval <= 0 {
			return fmt.Errorf("cleanup interval must be positive")
		}
		if c.Cleanup.AgentOfflineTimeout <= 0 {
			return fmt.Errorf("agent offline timeout must be positive")
		}
		if c.Cleanup.CompletedTaskMaxAge <= 0 {
			return fmt.Errorf("completed task max age must be positive")
		}
	}

	// Validate heartbeat configuration
	if c.Heartbeat.Interval <= 0 {
		return fmt.Errorf("heartbeat interval must be positive")
	}
	if c.Heartbeat.Timeout <= 0 {
		return fmt.Errorf("heartbeat timeout must be positive")
	}
	if c.Heartbeat.Timeout <= c.Heartbeat.Interval {
		return fmt.Errorf("heartbeat timeout must be greater than heartbeat interval")
	}

	return nil
}

// ConfigureLogging configures the logging system based on the config
func (c *Config) ConfigureLogging() error {
	// Set log level
	level, err := log.ParseLevel(c.Logging.Level)
	if err != nil {
		return fmt.Errorf("failed to parse log level: %w", err)
	}
	log.SetLevel(level)

	// Set log format
	switch c.Logging.Format {
	case "json":
		log.SetFormatter(&log.JSONFormatter{
			TimestampFormat: time.RFC3339,
		})
	case "text":
		log.SetFormatter(&log.TextFormatter{
			FullTimestamp:   true,
			TimestampFormat: time.RFC3339,
		})
	default:
		return fmt.Errorf("unsupported log format: %s", c.Logging.Format)
	}

	return nil
}

// GetListenAddress returns the address to listen on
func (c *Config) GetListenAddress() string {
	return fmt.Sprintf("%s:%s", c.Server.Host, c.Server.Port)
}

// ToMap converts the config to a map for logging/debugging
func (c *Config) ToMap() map[string]interface{} {
	return map[string]interface{}{
		"server": map[string]interface{}{
			"port": c.Server.Port,
			"host": c.Server.Host,
		},
		"logging": map[string]interface{}{
			"level":  c.Logging.Level,
			"format": c.Logging.Format,
		},
		"features": map[string]interface{}{
			"cross_codebase": c.Features.CrossCodebase,
			"persistence":    c.Features.Persistence,
		},
		"cleanup": map[string]interface{}{
			"enabled":                c.Cleanup.Enabled,
			"interval":               c.Cleanup.Interval.String(),
			"agent_offline_timeout":  c.Cleanup.AgentOfflineTimeout.String(),
			"completed_task_max_age": c.Cleanup.CompletedTaskMaxAge.String(),
		},
		"heartbeat": map[string]interface{}{
			"interval": c.Heartbeat.Interval.String(),
			"timeout":  c.Heartbeat.Timeout.String(),
		},
	}
}