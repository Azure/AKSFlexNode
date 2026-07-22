package nodebootstrap

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"time"

	"github.com/itchyny/gojq"

	"github.com/Azure/AKSFlexNode/pkg/config"
)

const (
	maxStartConfig = int64(16 << 20) // 16 MiB
	jqQueryTimeout = 10 * time.Second
)

var jqVariableNamePattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// ConfigRenderOptions controls live defaults and gojq transformations applied
// to a partial start configuration.
type ConfigRenderOptions struct {
	Queries    []string
	StringArgs []string
	JSONArgs   []string
	Hostname   string
	GOOS       string
	GOARCH     string
}

// FetchAndRenderConfig downloads and renders a partial start configuration.
func FetchAndRenderConfig(
	ctx context.Context,
	downloader *Downloader,
	source string,
	options ConfigRenderOptions,
) ([]byte, *config.Config, error) {
	if downloader == nil {
		return nil, nil, fmt.Errorf("start config downloader is required")
	}
	data, err := downloader.Fetch(ctx, source, maxStartConfig)
	if err != nil {
		return nil, nil, fmt.Errorf("download start config: %w", err)
	}
	return RenderConfig(ctx, data, options)
}

// RenderConfig defaults, transforms, and validates partial configuration JSON.
func RenderConfig(ctx context.Context, data []byte, options ConfigRenderOptions) ([]byte, *config.Config, error) {
	hostname := strings.TrimSpace(options.Hostname)
	if hostname == "" {
		var err error
		hostname, err = os.Hostname()
		if err != nil {
			return nil, nil, fmt.Errorf("get host name: %w", err)
		}
	}
	goos := options.GOOS
	if goos == "" {
		goos = runtime.GOOS
	}
	goarch := options.GOARCH
	if goarch == "" {
		goarch = runtime.GOARCH
	}

	partial, err := decodeJSONObject(data)
	if err != nil {
		return nil, nil, fmt.Errorf("parse partial start config: %w", err)
	}
	if err := applyLiveDefaults(partial, hostname); err != nil {
		return nil, nil, err
	}
	defaultedData, err := json.Marshal(partial)
	if err != nil {
		return nil, nil, fmt.Errorf("marshal partial start config: %w", err)
	}
	cfg, err := loadConfigData(defaultedData)
	if err != nil {
		return nil, nil, fmt.Errorf("default partial start config: %w", err)
	}

	defaulted, err := configAsJSONObject(cfg)
	if err != nil {
		return nil, nil, err
	}
	variables, values, err := jqVariables(options, hostname, cfg.Agent.NodeName, goos, goarch)
	if err != nil {
		return nil, nil, err
	}
	transformed, err := applyQueries(ctx, defaulted, options.Queries, variables, values)
	if err != nil {
		return nil, nil, err
	}
	transformedData, err := json.Marshal(transformed)
	if err != nil {
		return nil, nil, fmt.Errorf("marshal transformed start config: %w", err)
	}
	cfg, err = loadConfigData(transformedData)
	if err != nil {
		return nil, nil, fmt.Errorf("validate transformed start config: %w", err)
	}
	finalObject, err := configAsJSONObject(cfg)
	if err != nil {
		return nil, nil, err
	}
	finalData, err := json.MarshalIndent(finalObject, "", "  ")
	if err != nil {
		return nil, nil, fmt.Errorf("marshal final start config: %w", err)
	}
	finalData = append(finalData, '\n')
	return finalData, cfg, nil
}

func loadConfigData(data []byte) (*config.Config, error) {
	dir, err := os.MkdirTemp("", "aks-flex-node-config-")
	if err != nil {
		return nil, fmt.Errorf("create protected config staging directory: %w", err)
	}
	defer func() { _ = os.RemoveAll(dir) }()
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return nil, fmt.Errorf("stage protected config: %w", err)
	}
	return config.LoadConfig(path)
}

func applyLiveDefaults(root map[string]any, hostname string) error {
	agent, err := ensureObject(root, "agent")
	if err != nil {
		return err
	}
	if nodeName, _ := agent["nodeName"].(string); strings.TrimSpace(nodeName) == "" {
		agent["nodeName"] = strings.ToLower(strings.TrimSpace(hostname))
	}
	azure, ok := root["azure"].(map[string]any)
	if !ok {
		return nil
	}
	arc, ok := azure["arc"].(map[string]any)
	if !ok || arc["enabled"] != true {
		return nil
	}
	if machineName, _ := arc["machineName"].(string); strings.TrimSpace(machineName) == "" {
		arc["machineName"] = agent["nodeName"]
	}
	return nil
}

func ensureObject(root map[string]any, key string) (map[string]any, error) {
	if existing, found := root[key]; found {
		value, ok := existing.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("partial start config field %s must be a JSON object", key)
		}
		return value, nil
	}
	value := map[string]any{}
	root[key] = value
	return value, nil
}

func configAsJSONObject(cfg *config.Config) (map[string]any, error) {
	data, err := json.Marshal(cfg)
	if err != nil {
		return nil, fmt.Errorf("marshal defaulted start config: %w", err)
	}
	result, err := decodeJSONObject(data)
	if err != nil {
		return nil, fmt.Errorf("decode defaulted start config: %w", err)
	}
	// TargetCluster's derived fields are runtime helpers without JSON tags in
	// the existing config type. Keep materialization concerns local rather than
	// changing the public config structure.
	if azure, ok := result["azure"].(map[string]any); ok {
		if target, ok := azure["targetCluster"].(map[string]any); ok {
			delete(target, "Name")
			delete(target, "ResourceGroup")
			delete(target, "SubscriptionID")
		}
	}
	return result, nil
}

func decodeJSONObject(data []byte) (map[string]any, error) {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return nil, err
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			return nil, fmt.Errorf("multiple JSON values are not allowed")
		}
		return nil, fmt.Errorf("parse trailing JSON data: %w", err)
	}
	object, ok := value.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("configuration must be a JSON object")
	}
	return object, nil
}

func jqVariables(
	options ConfigRenderOptions,
	hostname, nodeName, goos, goarch string,
) ([]string, []any, error) {
	variables := []string{"$hostName", "$nodeName", "$os", "$arch"}
	values := []any{hostname, nodeName, goos, goarch}
	seen := map[string]struct{}{
		"hostName": {},
		"nodeName": {},
		"os":       {},
		"arch":     {},
	}
	if len(options.Queries) == 0 && (len(options.StringArgs) > 0 || len(options.JSONArgs) > 0) {
		return nil, nil, fmt.Errorf("--jq-arg and --jq-argjson require at least one --jq query")
	}
	for _, assignment := range options.StringArgs {
		name, value, err := parseJQAssignment(assignment)
		if err != nil {
			return nil, nil, fmt.Errorf("parse --jq-arg: %w", err)
		}
		if err := addJQVariable(&variables, &values, seen, name, value); err != nil {
			return nil, nil, err
		}
	}
	for _, assignment := range options.JSONArgs {
		name, raw, err := parseJQAssignment(assignment)
		if err != nil {
			return nil, nil, fmt.Errorf("parse --jq-argjson: %w", err)
		}
		value, err := decodeJSONValue(raw)
		if err != nil {
			return nil, nil, fmt.Errorf("parse --jq-argjson %s: %w", name, err)
		}
		if err := addJQVariable(&variables, &values, seen, name, value); err != nil {
			return nil, nil, err
		}
	}
	return variables, values, nil
}

func decodeJSONValue(raw string) (any, error) {
	decoder := json.NewDecoder(strings.NewReader(raw))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return nil, err
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			return nil, fmt.Errorf("multiple JSON values are not allowed")
		}
		return nil, err
	}
	return value, nil
}

func parseJQAssignment(assignment string) (string, string, error) {
	name, value, found := strings.Cut(assignment, "=")
	name = strings.TrimSpace(name)
	if !found || !jqVariableNamePattern.MatchString(name) {
		return "", "", fmt.Errorf("expected NAME=VALUE with a valid jq variable name")
	}
	return name, value, nil
}

func addJQVariable(variables *[]string, values *[]any, seen map[string]struct{}, name string, value any) error {
	if _, exists := seen[name]; exists {
		return fmt.Errorf("jq variable $%s is duplicated or reserved", name)
	}
	seen[name] = struct{}{}
	*variables = append(*variables, "$"+name)
	*values = append(*values, value)
	return nil
}

func applyQueries(
	ctx context.Context,
	input map[string]any,
	queries, variables []string,
	values []any,
) (map[string]any, error) {
	var current any = input
	for index, expression := range queries {
		query, err := gojq.Parse(expression)
		if err != nil {
			return nil, fmt.Errorf("parse --jq query %d: %w", index+1, err)
		}
		code, err := gojq.Compile(query, gojq.WithVariables(variables))
		if err != nil {
			return nil, fmt.Errorf("compile --jq query %d: %w", index+1, err)
		}
		queryCtx, cancel := context.WithTimeout(ctx, jqQueryTimeout)
		iterator := code.RunWithContext(queryCtx, current, values...)
		result, ok := iterator.Next()
		if !ok {
			cancel()
			return nil, fmt.Errorf("--jq query %d returned no result", index+1)
		}
		if queryErr, ok := result.(error); ok {
			cancel()
			return nil, fmt.Errorf("run --jq query %d: %w", index+1, queryErr)
		}
		if extra, ok := iterator.Next(); ok {
			cancel()
			if queryErr, ok := extra.(error); ok {
				return nil, fmt.Errorf("run --jq query %d: %w", index+1, queryErr)
			}
			return nil, fmt.Errorf("--jq query %d returned multiple results", index+1)
		}
		cancel()
		object, ok := result.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("--jq query %d must return a JSON object", index+1)
		}
		current = object
	}
	object, ok := current.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("transformed start config must be a JSON object")
	}
	return object, nil
}
