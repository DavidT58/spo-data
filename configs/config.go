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
// are expected to share the same blockfrost_address; the first non-empty address
// is used.
//
// A file that fails to parse is SKIPPED (not fatal) and reported in the returned
// `skipped` slice, so one malformed operator file can never take the whole
// monitor down. A hard error is returned only if the directory is unreadable or
// no usable pools are found at all.
func LoadAllConfigs(dir string) (cfg Config, skipped []string, err error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return Config{}, nil, fmt.Errorf("failed to read config dir %q: %v", dir, err)
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
		fileCfg, ferr := LoadConfigFromYAML(path)
		if ferr != nil {
			// Skip and report — never abort the whole monitor for one bad file.
			skipped = append(skipped, fmt.Sprintf("%s: %v", name, ferr))
			continue
		}
		// A valid operator file must declare at least a blockfrost address.
		if fileCfg.BlockFrostAddress == "" && len(fileCfg.Pools) == 0 {
			continue
		}
		operator := strings.TrimSuffix(name, filepath.Ext(name))
		for i := range fileCfg.Pools {
			fileCfg.Pools[i].Operator = operator
			merged.Pools = append(merged.Pools, fileCfg.Pools[i])
		}
		if merged.BlockFrostAddress == "" && fileCfg.BlockFrostAddress != "" {
			merged.BlockFrostAddress = fileCfg.BlockFrostAddress
		}
	}

	if merged.BlockFrostAddress == "" {
		return Config{}, skipped, fmt.Errorf("no blockfrost_address found in any config under %q", dir)
	}
	if len(merged.Pools) == 0 {
		return Config{}, skipped, fmt.Errorf("no pools found in any config under %q", dir)
	}

	return merged, skipped, nil
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
