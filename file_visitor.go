package main

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/bmatcuk/doublestar/v4"
)

type RegexResult []string

func (res RegexResult) applyOnTemplate(template string) string {
	out := template
	for i, match := range res {
		out = strings.ReplaceAll(out, fmt.Sprintf("$%d", i), match)
	}
	return out
}

func (res RegexResult) applyOnTemplates(templates []string) (out []string) {
	for _, template := range templates {
		out = append(out, res.applyOnTemplate(template))
	}
	return
}

func applyActions(
	actions *RuleActions,
	file string,
	file_data **string,
	file_relations *[]string,
	python_mod_resolver *PythonModuleResolver,
	config *Config,
	args *Args,
	base_dir string,
	regex_result RegexResult,
) error {
	// Visit files
	for _, visit := range regex_result.applyOnTemplates(actions.Visit.items) {
		visit_files_chunk, err := doublestar.Glob(
			os.DirFS(base_dir),
			visit,
			doublestar.WithFilesOnly(),
			doublestar.WithFailOnIOErrors(),
		)
		if err != nil {
			return fmt.Errorf("error while visiting '%s': %v", visit, err)
		}
		*file_relations = append(*file_relations, visit_files_chunk...)
	}

	// Visit siblings
	path_iter := filepath.Dir(file)
	for _, visit := range regex_result.applyOnTemplates(actions.VisitSiblings.items) {
		visit_files_chunk, err := doublestar.Glob(
			os.DirFS(filepath.Join(base_dir, path_iter)),
			visit,
			doublestar.WithFilesOnly(),
			doublestar.WithFailOnIOErrors(),
		)
		if err != nil {
			return fmt.Errorf("error while visiting sibling '%s': %v", visit, err)
		}
		for _, visit_file := range visit_files_chunk {
			*file_relations = append(*file_relations, filepath.Join(path_iter, visit_file))
		}
	}

	// Visit grand siblings
	for path_iter != "." {
		for _, visit := range regex_result.applyOnTemplates(actions.VisitGrandSiblings.items) {
			visit_files_chunk, err := doublestar.Glob(
				os.DirFS(filepath.Join(base_dir, path_iter)),
				visit,
				doublestar.WithFilesOnly(),
				doublestar.WithFailOnIOErrors(),
			)
			if err != nil {
				return fmt.Errorf(
					"error while visiting grand sibling '%s' at '%s': %v",
					visit,
					path_iter,
					err,
				)
			}
			for _, visit_file := range visit_files_chunk {
				*file_relations = append(
					*file_relations,
					filepath.Join(path_iter, visit_file),
				)
			}
		}
		path_iter = filepath.Dir(path_iter)
	}

	// Visit imported Python modules
	if actions.VisitImportedPythonModules || len(actions.VisitPythonAllSubmodulesFor.items) != 0 {
		// Read file
		if *file_data == nil {
			file_data_bytes, err := os.ReadFile(filepath.Join(base_dir, file))
			if err != nil {
				return fmt.Errorf("error while reading python file: %v", err)
			}
			file_data_str := string(file_data_bytes)
			*file_data = &file_data_str
		}

		// Parse all import statements
		pyimports := []string{}
		pyimports_idents := map[string]string{}
		for _, match := range python_import_parser_simple.FindAllStringSubmatch(**file_data, -1) {
			pyimports = append(pyimports, match[1])
			pyimports_idents[match[1]] = match[1]
		}
		for _, match := range python_import_parser_from.FindAllStringSubmatch(**file_data, -1) {
			pyimports = append(pyimports, match[1])
			for _, import_ident := range python_import_parser_ident.FindAllStringSubmatch(
				match[2], -1,
			) {
				full_mod_name := match[1] + "." + import_ident[0]
				pyimports = append(pyimports, full_mod_name)
				pyimports_idents[import_ident[0]] = full_mod_name
			}
		}

		// Visit all submodules of a given python module by name
		if len(actions.VisitPythonAllSubmodulesFor.items) != 0 {
			for _, mod_name := range regex_result.applyOnTemplates(
				actions.VisitPythonAllSubmodulesFor.items,
			) {
				found_in_root_pkg := false
				var full_mod_name string
				for _, root_package := range config.RootPythonPackages.items {
					if strings.HasPrefix(mod_name, root_package+".") || mod_name == root_package {
						found_in_root_pkg = true
						full_mod_name = mod_name
					}
				}

				if !found_in_root_pkg {
					var ok bool
					full_mod_name, ok = pyimports_idents[mod_name]
					if !ok {
						return fmt.Errorf("module ident '%s' not found", mod_name)
					}
				}

				if args.Verbose {
					log.Println("Visiting all submodules of:", mod_name, "->", full_mod_name)
				}
				dir_path := strings.ReplaceAll(full_mod_name, ".", "/")

				visit_files_chunk, err := doublestar.Glob(
					os.DirFS(base_dir),
					dir_path+"/**/*.py",
					doublestar.WithFilesOnly(),
					doublestar.WithFailOnIOErrors(),
				)
				if err != nil {
					return fmt.Errorf("error while visiting submodule '%s': %v", full_mod_name, err)
				}
				*file_relations = append(*file_relations, visit_files_chunk...)
			}
		}

		// Resolve the imports
		for _, module := range pyimports {
			paths, err := python_mod_resolver.Resolve(module, config, base_dir)
			if err != nil {
				return fmt.Errorf("error while resolving python module '%s': %v", module, err)
			}
			*file_relations = append(*file_relations, paths.Paths...)
		}
	}

	return nil
}

func checkExcludePatterns(exclude_patterns []string, file string) (bool, error) {
	for _, excluded_file := range exclude_patterns {
		match, err := doublestar.Match(excluded_file, file)
		if err != nil {
			return false, fmt.Errorf(
				"error matching exclusion '%s' on '%s': %v",
				excluded_file,
				file,
				err,
			)
		}
		if match {
			return true, nil
		}
	}
	return false, nil
}

func visitFile(
	file string,
	file_relations *[]string,
	python_mod_resolver *PythonModuleResolver,
	regex_cache map[string]*regexp.Regexp,
	config *Config,
	args *Args,
	base_dir string,
) error {
	// Ignore globally excluded files
	excluded, err := checkExcludePatterns(config.GlobalExclude.items, file)
	if err != nil {
		return fmt.Errorf("error checking global_exclude: %v", err)
	}
	if excluded {
		return nil
	}

	if args.Verbose {
		log.Println("Visiting:", file)
	}

	for rule_pattern, path_rules := range config.PathRules {
		match, err := doublestar.Match(rule_pattern, file)
		var file_data *string
		if err != nil {
			return fmt.Errorf("error matching rule '%s': %v", rule_pattern, err)
		}
		if match {
			if args.Verbose {
				log.Println("Matched rule:", rule_pattern)
			}

			err := applyActions(
				&path_rules.Actions,
				file,
				&file_data,
				file_relations,
				python_mod_resolver,
				config,
				args,
				base_dir,
				nil,
			)
			if err != nil {
				return fmt.Errorf(
					"error while running path_rule '%s': %v",
					rule_pattern,
					err,
				)
			}

			// Apply Regex Rules
			for regex_rule_pattern, regex_actions := range path_rules.RegexRules {
				// Check if the file is excluded
				excluded, err := checkExcludePatterns(regex_actions.Exclude.items, file)
				if err != nil {
					return fmt.Errorf(
						"error checking exclude of '%s' in rule '%s': %v",
						regex_rule_pattern,
						rule_pattern,
						err,
					)
				}
				if excluded {
					continue
				}
				// Read file
				if file_data == nil {
					file_data_bytes, err := os.ReadFile(filepath.Join(base_dir, file))
					if err != nil {
						return fmt.Errorf(
							"error while running path_rule '%s': error while reading python file: %v",
							rule_pattern,
							err,
						)
					}
					file_data_str := string(file_data_bytes)
					file_data = &file_data_str
				}
				// Compile the regex pattern
				if _, ok := regex_cache[regex_rule_pattern]; !ok {
					regex_pattern, err := regexp.Compile(regex_rule_pattern)
					if err != nil {
						return fmt.Errorf(
							"error while running path_rule '%s': error while compiling regex rule '%s': %v",
							rule_pattern,
							regex_rule_pattern,
							err,
						)
					}
					regex_cache[regex_rule_pattern] = regex_pattern
				}
				regex_pattern := regex_cache[regex_rule_pattern]
				// Find all matches
				regex_matches := regex_pattern.FindAllStringSubmatch(*file_data, -1)
				for _, regex_match := range regex_matches {
					if args.Verbose {
						log.Println("Matched regex rule:", file, regex_rule_pattern, regex_match)
					}
					err := applyActions(
						&regex_actions,
						file,
						&file_data,
						file_relations,
						python_mod_resolver,
						config,
						args,
						base_dir,
						regex_match,
					)
					if err != nil {
						return fmt.Errorf(
							"error while running path_rule '%s': error while running regex rule '%s': %v",
							rule_pattern,
							regex_rule_pattern,
							err,
						)
					}
				}
			}
		}
	}
	return nil
}

func VisitRecursively(
	all_files_set map[string]bool,
	file_relation_map map[string][]string,
	input_files []string,
	config *Config,
	args *Args,
	base_dir string,
) error {
	regex_cache := map[string]*regexp.Regexp{}
	python_mod_resolver := PythonModuleResolver{
		cache: map[string]*PythonModuleResolverResult{},
	}

	// Loop until we have no more files to visit
	for {
		related_files := []string{}
		if args.Verbose {
			log.Println("---")
		}

		// Visit each file
		for _, file := range input_files {
			if all_files_set[file] {
				continue
			}
			all_files_set[file] = true
			file_relations := config.GlobalDeps.items

			err := visitFile(file, &file_relations, &python_mod_resolver, regex_cache, config, args, base_dir)
			if err != nil {
				return fmt.Errorf("error while visiting file '%s': %v", file, err)
			}

			// Sort, dedup, and save the related files
			slices.Sort(file_relations)
			file_relations = slices.Compact(file_relations)
			file_relation_map[file] = file_relations
			related_files = append(related_files, file_relations...)
		}

		if len(related_files) != 0 {
			// Sort, dedup, and send the slice to the queue
			slices.Sort(related_files)
			input_files = slices.Compact(related_files)
		} else {
			return nil
		}
	}
}
