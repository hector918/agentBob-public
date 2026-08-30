package config

import (
	"reflect"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// TestConfigTemplateCoversAllSections guards against config drift: every
// top-level yaml section on Config MUST appear in ConfigYAMLExample, so
// `bob config init` documents the full config surface and a new-machine
// seed never hides a "param you just have to know". When this fails, add the
// missing section (with its default + a one-line comment) to ConfigYAMLExample.
func TestConfigTemplateCoversAllSections(t *testing.T) {
	var top map[string]yaml.Node
	if err := yaml.Unmarshal([]byte(ConfigYAMLExample), &top); err != nil {
		t.Fatalf("ConfigYAMLExample is not valid YAML: %v", err)
	}
	ty := reflect.TypeOf(Config{})
	for i := 0; i < ty.NumField(); i++ {
		name := strings.Split(ty.Field(i).Tag.Get("yaml"), ",")[0]
		if name == "" || name == "-" {
			continue
		}
		if _, ok := top[name]; !ok {
			t.Errorf("ConfigYAMLExample is missing top-level section %q — add it (default + one-line comment) so `bob config init` documents the full surface", name)
		}
	}
}
