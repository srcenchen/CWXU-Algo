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

func TestGatewayCorsConfigMatchesAppExampleAndAllowsOnlyConfiguredOrigins(t *testing.T) {
	root := filepath.Clean("..")
	app := gatewayCorsConfig(t, filepath.Join(root, "app/gateway/cmd/gateway/config.yaml"))
	deploy := gatewayCorsConfig(t, filepath.Join(root, "deploy/config/gateway.yaml"))
	if !reflect.DeepEqual(deploy, app) {
		t.Fatalf("gateway CORS differs between app example and deploy template\napp:    %#v\ndeploy: %#v", app, deploy)
	}
	want := gatewayCorsOptions{
		Type:         "type.googleapis.com/gateway.middleware.cors.v1.Cors",
		AllowOrigins: []string{"https://algo.zhiyuansofts.cn"},
		AllowHeaders: []string{"Content-Type", "X-GoAlgo-Plugin-Token", "X-GoAlgo-Sync-Session"},
	}
	if !reflect.DeepEqual(app, want) {
		t.Fatalf("gateway CORS = %#v, want %#v", app, want)
	}
}

type gatewayCorsOptions struct {
	Type         string   `yaml:"@type"`
	AllowOrigins []string `yaml:"allowOrigins"`
	AllowHeaders []string `yaml:"allowHeaders"`
}

func gatewayCorsConfig(t *testing.T, path string) gatewayCorsOptions {
	t.Helper()
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var config struct {
		Middlewares []struct {
			Name    string             `yaml:"name"`
			Options gatewayCorsOptions `yaml:"options"`
		} `yaml:"middlewares"`
	}
	if err := yaml.Unmarshal(content, &config); err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	var corsMiddlewares []gatewayCorsOptions
	for _, middleware := range config.Middlewares {
		if middleware.Name == "cors" {
			corsMiddlewares = append(corsMiddlewares, middleware.Options)
		}
	}
	if len(corsMiddlewares) != 1 {
		t.Fatalf("%s must configure exactly one cors middleware, got %d", path, len(corsMiddlewares))
	}
	return corsMiddlewares[0]
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
