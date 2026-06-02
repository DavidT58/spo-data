package configs

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

type PoolConfig struct {
	Name   string `json:"name" yaml:"name"`
	PoolID string `json:"poolID" yaml:"poolID"`
	// Operator is not read from YAML; it is set to the config filename at load
	// time so alerts can be attributed to the operator who owns the pool.
	Operator string `json:"operator" yaml:"-"`
}

type Config struct {
	Pools             []PoolConfig `json:"pools"`
	BlockFrostAddress string       `json:"blockfrost_address" yaml:"blockfrost_address"`
}

func LoadConfigFromYAML(filePath string) (Config, error) {
	file, err := os.Open(filePath)

	if err != nil {
		return Config{}, fmt.Errorf("failed to open config file: %v", err)
	}
	defer file.Close()

	var config Config

	decoder := yaml.NewDecoder(file)
	if err := decoder.Decode(&config); err != nil {
		return Config{}, fmt.Errorf("failed to decode config file: %v", err)
	}

	return config, nil
}

// LoadAllConfigs reads every operator config file in dir, tags each pool with
// its operator (the filename), and merges them into a single Config. All files
// are expected to share the same blockfrost_address; if they diverge, the first
// non-empty address is used and a warning is returned in the error-free path via
// stderr-style logging by the caller (we just pick the first here).
func LoadAllConfigs(dir string) (Config, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return Config{}, fmt.Errorf("failed to read config dir %q: %v", dir, err)
	}

	// Deterministic operator ordering.
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		// Skip hidden files and obvious non-config files.
		if strings.HasPrefix(name, ".") {
			continue
		}
		names = append(names, name)
	}
	sort.Strings(names)

	merged := Config{}
	for _, name := range names {
		path := filepath.Join(dir, name)
		cfg, err := LoadConfigFromYAML(path)
		if err != nil {
			return Config{}, fmt.Errorf("failed to load %q: %v", path, err)
		}
		// A valid operator file must declare at least a blockfrost address.
		if cfg.BlockFrostAddress == "" && len(cfg.Pools) == 0 {
			continue
		}
		operator := strings.TrimSuffix(name, filepath.Ext(name))
		for i := range cfg.Pools {
			cfg.Pools[i].Operator = operator
			merged.Pools = append(merged.Pools, cfg.Pools[i])
		}
		if merged.BlockFrostAddress == "" && cfg.BlockFrostAddress != "" {
			merged.BlockFrostAddress = cfg.BlockFrostAddress
		}
	}

	if merged.BlockFrostAddress == "" {
		return Config{}, fmt.Errorf("no blockfrost_address found in any config under %q", dir)
	}
	if len(merged.Pools) == 0 {
		return Config{}, fmt.Errorf("no pools found in any config under %q", dir)
	}

	return merged, nil
}

func (c Config) getPools() []map[string]string {
	var result []map[string]string
	for _, pool := range c.Pools {
		result = append(result, map[string]string{
			"name":   pool.Name,
			"poolID": pool.PoolID,
		})
	}
	return result
}
