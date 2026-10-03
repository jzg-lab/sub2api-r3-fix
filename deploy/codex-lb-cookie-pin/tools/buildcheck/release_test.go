package buildcheck

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

type releaseConfig struct {
	Before struct {
		Hooks []string `yaml:"hooks"`
	} `yaml:"before"`
	Archives []struct {
		Files           []yaml.Node `yaml:"files"`
		Format          string      `yaml:"format"`
		Formats         []string    `yaml:"formats"`
		FormatOverrides []struct {
			Goos    string   `yaml:"goos"`
			Format  string   `yaml:"format"`
			Formats []string `yaml:"formats"`
		} `yaml:"format_overrides"`
	} `yaml:"archives"`
}

func TestReleaseUsesCurrentArchiveFormats(t *testing.T) {
	for _, name := range []string{".goreleaser.yaml", ".goreleaser.simple.yaml"} {
		t.Run(name, func(t *testing.T) {
			data, err := os.ReadFile(filepath.Join("../../../..", name))
			if err != nil {
				t.Fatal(err)
			}
			var config releaseConfig
			if err := yaml.Unmarshal(data, &config); err != nil {
				t.Fatal(err)
			}
			if len(config.Archives) == 0 {
				t.Fatal("release has no binary archives")
			}
			for _, archive := range config.Archives {
				if archive.Format != "" || !slices.Equal(archive.Formats, []string{"tar.gz"}) {
					t.Fatal("default archives must use formats: [tar.gz], not deprecated format")
				}
				if len(archive.FormatOverrides) != 1 {
					t.Fatal("release must have exactly one Windows ZIP override")
				}
				override := archive.FormatOverrides[0]
				if override.Goos != "windows" || override.Format != "" ||
					!slices.Equal(override.Formats, []string{"zip"}) {
					t.Fatal("Windows archives must use formats: [zip], not deprecated format")
				}
			}
		})
	}
}

func validateRescueRelease(data []byte, version string) error {
	var config releaseConfig
	if err := yaml.Unmarshal(data, &config); err != nil {
		return err
	}
	if !slices.Contains(config.Before.Hooks, "sh deploy/codex-lb-cookie-pin/build.sh") {
		return fmt.Errorf("release must build and validate the coupled rescue plugin")
	}
	if len(config.Archives) == 0 {
		return fmt.Errorf("release has no binary archives")
	}
	wanted := "deploy/codex-lb-cookie-pin/dist/lyunlong-codex-lb-cookie-pin-" + version + ".s2plugin"
	broadDeploySource := func(src string) bool {
		return slices.Contains([]string{"deploy", "deploy/", "deploy/*", "deploy/**", "deploy/**/*"}, src)
	}
	for i, archive := range config.Archives {
		found := 0
		for _, node := range archive.Files {
			if node.Kind == yaml.ScalarNode &&
				(broadDeploySource(node.Value) || strings.HasPrefix(node.Value, "deploy/codex-lb-cookie-pin")) {
				return fmt.Errorf("archive %d recursively includes rescue sources or unpinned runtimes", i)
			}
			if node.Kind != yaml.MappingNode {
				continue
			}
			var entry struct {
				Src         string `yaml:"src"`
				Dst         string `yaml:"dst"`
				StripParent bool   `yaml:"strip_parent"`
			}
			if err := node.Decode(&entry); err != nil {
				return err
			}
			if broadDeploySource(entry.Src) ||
				(strings.HasPrefix(entry.Src, "deploy/codex-lb-cookie-pin") &&
					entry.Src != wanted && entry.Src != "deploy/codex-lb-cookie-pin/README.md") {
				return fmt.Errorf("archive %d includes rescue sources, runtimes or stale packages", i)
			}
			if entry.Src == wanted {
				if entry.Dst != "plugins" || !entry.StripParent {
					return fmt.Errorf("archive %d puts the rescue package outside plugins", i)
				}
				found++
			}
		}
		if found != 1 {
			return fmt.Errorf("archive %d must include exactly one current rescue package under plugins", i)
		}
	}
	return nil
}

func TestReleaseIncludesCurrentRescuePackage(t *testing.T) {
	data, err := os.ReadFile("../../manifest.source.json")
	if err != nil {
		t.Fatal(err)
	}
	var manifest struct {
		Version string `json:"version"`
	}
	if err := json.Unmarshal(data, &manifest); err != nil {
		t.Fatal(err)
	}
	if manifest.Version == "" {
		t.Fatal("rescue manifest is missing a version")
	}
	for _, name := range []string{".goreleaser.yaml", ".goreleaser.simple.yaml"} {
		t.Run(name, func(t *testing.T) {
			data, err := os.ReadFile(filepath.Join("../../../..", name))
			if err != nil {
				t.Fatal(err)
			}
			if err := validateRescueRelease(data, manifest.Version); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestReleaseRejectsIncompleteRescueBundle(t *testing.T) {
	hook := "before:\n  hooks:\n    - sh deploy/codex-lb-cookie-pin/build.sh\n"
	entry := func(version, destination string) string {
		return "archives:\n  - files:\n      - src: deploy/codex-lb-cookie-pin/dist/lyunlong-codex-lb-cookie-pin-" +
			version + ".s2plugin\n        dst: " + destination + "\n        strip_parent: true\n"
	}
	for _, tc := range []struct {
		name string
		data string
	}{
		{"missing_build_hook", entry("0.3.7", "plugins")},
		{"missing_archives", hook},
		{"missing_package", hook + "archives:\n  - files:\n      - README.md\n"},
		{"stale_package", hook + entry("0.3.6", "plugins")},
		{"wildcard_can_bundle_stale_versions", hook + entry("*", "plugins")},
		{"wrong_destination", hook + entry("0.3.7", ".")},
		{"malformed_yaml", hook + "archives: ["},
		{"recursive_deploy_glob", hook + entry("0.3.7", "plugins") + "      - deploy/*\n"},
		{"recursive_deploy_directory", hook + entry("0.3.7", "plugins") + "      - src: deploy\n"},
		{"recursive_deploy_mapping", hook + entry("0.3.7", "plugins") + "      - src: deploy/**\n"},
		{"extra_stale_package", hook + entry("0.3.7", "plugins") + "      - deploy/codex-lb-cookie-pin/dist/lyunlong-codex-lb-cookie-pin-0.3.6.s2plugin\n"},
		{"raw_plugin_runtimes", hook + entry("0.3.7", "plugins") + "      - src: deploy/codex-lb-cookie-pin/dist/runtimes/*\n"},
		{"duplicate_current_package", hook + entry("0.3.7", "plugins") +
			"      - src: deploy/codex-lb-cookie-pin/dist/lyunlong-codex-lb-cookie-pin-0.3.7.s2plugin\n        dst: plugins\n        strip_parent: true\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := validateRescueRelease([]byte(tc.data), "0.3.7"); err == nil {
				t.Fatal("incomplete or stale rescue bundle was accepted")
			}
		})
	}
}
