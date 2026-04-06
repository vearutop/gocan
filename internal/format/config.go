// Package format defines formatting configuration and helpers.
package format

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
)

// DefaultConfig returns the built-in formatting configuration.
func DefaultConfig() Config {
	// Default order follows the example given by the user.
	return Config{
		Order: []Rule{
			{Kind: "packageMainFunc"},
			{Kind: "const", Exported: true},
			{Kind: "var", Exported: true},
			{Kind: "func", Exported: true},
			{Kind: "constructor", Exported: true},
			{Kind: "type", Exported: true},
			{Kind: "receiver", Exported: true, ExportedMethod: true},
			{Kind: "receiver", Exported: true, ExportedMethod: false},
			{Kind: "const", Exported: false},
			{Kind: "var", Exported: false},
			{Kind: "func", Exported: false},
			{Kind: "constructor", Exported: false},
			{Kind: "type", Exported: false},
			{Kind: "receiver", Exported: false, ExportedMethod: true},
			{Kind: "receiver", Exported: false, ExportedMethod: false},
		},
		HelperAttachment: true,
	}
}

// LoadConfig reads formatting configuration from JSON file.
func LoadConfig(path string) (Config, error) {
	if path == "" {
		return DefaultConfig(), nil
	}

	b, err := os.ReadFile(path)
	if err != nil {
		return Config{}, err
	}

	var cfg Config
	if err := json.Unmarshal(b, &cfg); err != nil {
		return Config{}, fmt.Errorf("parse config: %w", err)
	}

	if len(cfg.Order) == 0 {
		return Config{}, errors.New("config order is empty")
	}

	return cfg, nil
}

// Config defines the declaration ordering rules and exclusions.
type Config struct {
	Order            []Rule   `json:"order"`
	HelperAttachment bool     `json:"helperAttachment,omitempty"`
	Exclude          []string `json:"exclude,omitempty"`
}

// Rule defines ordering for a declaration kind.
type Rule struct {
	Kind           string `json:"kind"`
	Exported       bool   `json:"exported,omitempty"`
	ExportedMethod bool   `json:"exportedMethod,omitempty"`
}
