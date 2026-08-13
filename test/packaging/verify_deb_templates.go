package main

import (
	"bytes"
	"fmt"
	"os"
	"strings"
	"text/template"

	"go.yaml.in/yaml/v3"
)

type goreleaserConfig struct {
	NFPMs []nfpmConfig `yaml:"nfpms"`
}

type nfpmConfig struct {
	ID               string `yaml:"id"`
	FileNameTemplate string `yaml:"file_name_template"`
}

type templateFields struct {
	ConventionalFileName string
}

// Keep both separators in the input so a no-op template comment cannot satisfy
// this check without rendering the safe filename.
const conventionalFileName = "0.17.0~rc.1+build.5"

func main() {
	if len(os.Args) != 2 {
		fail("usage: verify_deb_templates <goreleaser-config>")
	}

	config, err := os.ReadFile(os.Args[1])
	if err != nil {
		fail("read config: %v", err)
	}

	var parsed goreleaserConfig
	if err := yaml.Unmarshal(config, &parsed); err != nil {
		fail("parse config: %v", err)
	}

	for _, packageName := range []string{"piperd", "piper"} {
		templateText := ""
		for _, nfpm := range parsed.NFPMs {
			if nfpm.ID == packageName {
				templateText = nfpm.FileNameTemplate
				break
			}
		}
		if templateText == "" {
			fail("nfpms %s template is missing", packageName)
		}

		got, err := render(templateText, packageName+"_"+conventionalFileName+"_amd64.deb")
		if err != nil {
			fail("render nfpms %s template: %v", packageName, err)
		}
		expected := packageName + "_0.17.0.rc.1.build.5_amd64.deb"
		if got != expected {
			fail("nfpms %s template rendered %q, want %q", packageName, got, expected)
		}
	}
}

func render(templateText, conventionalFileName string) (string, error) {
	tmpl, err := template.New("file_name_template").Funcs(template.FuncMap{
		"replace": strings.ReplaceAll,
	}).Parse(templateText)
	if err != nil {
		return "", err
	}

	var rendered bytes.Buffer
	err = tmpl.Execute(&rendered, templateFields{ConventionalFileName: conventionalFileName})
	return rendered.String(), err
}

func fail(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "verify_deb_templates: "+format+"\n", args...)
	os.Exit(1)
}
