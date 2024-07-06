package main

import (
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

type StringOrStringArr struct {
	items []string
}

func (res *StringOrStringArr) UnmarshalYAML(unmarshal func(interface{}) error) error {
	var as_list []string
	err := unmarshal(&as_list)
	if err == nil {
		res.items = as_list
		return nil
	}
	var as_str string
	if err := unmarshal(&as_str); err == nil {
		res.items = []string{as_str}
		return nil
	}
	return fmt.Errorf("expected string or list of strings: %v", err)
}

type RuleActions struct {
	Visit                       StringOrStringArr
	VisitSiblings               StringOrStringArr `yaml:"visit_siblings"`
	VisitGrandSiblings          StringOrStringArr `yaml:"visit_grand_siblings"`
	VisitImportedPythonModules  bool              `yaml:"visit_imported_python_modules"`
	VisitPythonAllSubmodulesFor StringOrStringArr `yaml:"visit_python_all_submodules_for"`
	Exclude                     StringOrStringArr
}

type PathRule struct {
	Actions    RuleActions            `yaml:",inline"`
	RegexRules map[string]RuleActions `yaml:"regex_rules"`
}

type Config struct {
	Basedir            string
	Inputs             StringOrStringArr
	GlobalDeps         StringOrStringArr   `yaml:"global_deps"`
	GlobalExclude      StringOrStringArr   `yaml:"global_exclude"`
	RootPythonPackages StringOrStringArr   `yaml:"root_python_packages"`
	PathRules          map[string]PathRule `yaml:"path_rules"`
}

// Load the yaml config
func LoadConfig(path string) (*Config, error) {
	// Read the config file
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("failed to open config file: %w", err)
	}
	defer file.Close()

	// Decode the YAML data
	var config Config
	decoder := yaml.NewDecoder(file)
	decoder.KnownFields(true)
	err = decoder.Decode(&config)
	if err != nil {
		return nil, fmt.Errorf("failed to decode config file: %w", err)
	}

	return &config, nil
}
