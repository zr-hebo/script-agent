package runner

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

const MaxParamsEnvBytes = 64 << 10

// paramsEnvironment converts the normalized Shell parameter snapshot, without
// changing the supervisor's environment. Only top-level keys become PARAM_* variables.
func paramsEnvironment(params map[string]any) ([]string, error) {
	keys := make([]string, 0, len(params))
	for key := range params {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	env := make([]string, 0, len(keys))
	seen := make(map[string]string, len(keys))
	total := 0
	for _, key := range keys {
		if !validParamKey(key) {
			return nil, fmt.Errorf("params key %q must match [A-Za-z_][A-Za-z0-9_]* for environment mapping", key)
		}
		name := "PARAM_" + strings.ToUpper(key)
		if previous, ok := seen[name]; ok {
			return nil, fmt.Errorf("params keys %q and %q both map to %s", previous, key, name)
		}
		seen[name] = key
		value, isString := params[key].(string)
		if !isString {
			encoded, err := json.Marshal(params[key])
			if err != nil {
				return nil, fmt.Errorf("params key %q cannot be encoded for environment: %w", key, err)
			}
			value = string(encoded)
		}
		if strings.ContainsRune(value, 0) {
			return nil, fmt.Errorf("params key %q contains NUL, which is not allowed in environment values", key)
		}
		entry := name + "=" + value
		total += len(entry) + 1 // Include the terminating NUL used by exec.
		if total > MaxParamsEnvBytes {
			return nil, fmt.Errorf("params environment exceeds %d bytes", MaxParamsEnvBytes)
		}
		env = append(env, entry)
	}
	return env, nil
}

func validParamKey(key string) bool {
	if key == "" {
		return false
	}
	for i := 0; i < len(key); i++ {
		c := key[i]
		if c == '_' || c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || i > 0 && c >= '0' && c <= '9' {
			continue
		}
		return false
	}
	return true
}
