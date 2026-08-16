package deploy

import (
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestAppExamplesAndDeployTemplatesHaveMatchingKeyPaths(t *testing.T) {
	root := filepath.Clean("..")
	for _, pair := range [][2]string{
		{"app/user/configs/config.example.yaml", "deploy/config/user.yaml"},
		{"app/core_data/configs/config.example.yaml", "deploy/config/core-data.yaml"},
		{"app/agent/configs/config.example.yaml", "deploy/config/agent.yaml"},
	} {
		want := yamlKeyPaths(t, filepath.Join(root, pair[0]))
		got := yamlKeyPaths(t, filepath.Join(root, pair[1]))
		if !reflect.DeepEqual(got, want) {
			t.Errorf("config key paths differ for %s and %s\napp:    %v\ndeploy: %v", pair[0], pair[1], want, got)
		}
	}
}

func TestConfigTemplatesExcludeRuntimeBusinessSettings(t *testing.T) {
	root := filepath.Clean("..")
	for _, path := range []string{
		"app/user/configs/config.example.yaml", "deploy/config/user.yaml",
		"app/core_data/configs/config.example.yaml", "deploy/config/core-data.yaml",
		"app/agent/configs/config.example.yaml", "deploy/config/agent.yaml",
	} {
		paths := yamlKeyPaths(t, filepath.Join(root, path))
		for _, key := range paths {
			if key == "server.config_encryption_key" {
				t.Errorf("%s must not offer legacy config_encryption_key", path)
			}
		}
		for _, retired := range []string{"smtp", "agent", "ai_analyze", "upyun", "oj", "payfm", "backup"} {
			for _, key := range paths {
				if key == retired || len(key) > len(retired) && key[:len(retired)+1] == retired+"." {
					t.Errorf("%s still contains retired runtime key %s", path, key)
				}
			}
		}
	}
}

func yamlKeyPaths(t *testing.T, path string) []string {
	t.Helper()
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var value map[string]any
	if err := yaml.Unmarshal(content, &value); err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	var out []string
	var walk func(string, map[string]any)
	walk = func(prefix string, values map[string]any) {
		for key, child := range values {
			path := key
			if prefix != "" {
				path = prefix + "." + key
			}
			out = append(out, path)
			if nested, ok := child.(map[string]any); ok {
				walk(path, nested)
			}
		}
	}
	walk("", value)
	sort.Strings(out)
	return out
}
