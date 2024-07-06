package main

import (
	"log"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

var python_import_parser_simple = regexp.MustCompile(`(?m:^ *import ([^ \n]+))`)
var python_import_parser_from = regexp.MustCompile(`(?m:^ *from ([^ \n]+) import (\([^)]+\)|[^\n]+))`)
var python_import_parser_ident = regexp.MustCompile(`[A-Za-z_][A-Za-z0-9_]*`)

type PythonModuleResolverResult struct {
	Paths []string
}

type PythonModuleResolver struct {
	cache map[string]*PythonModuleResolverResult
}

func (res *PythonModuleResolver) Resolve(
	module string, config *Config, base_dir string,
) (*PythonModuleResolverResult, error) {
	if cached := res.cache[module]; cached != nil {
		return cached, nil
	}

	// Filter to specified root modules
	allowed := false
	for _, root_python_package := range config.RootPythonPackages.items {
		if strings.HasPrefix(module, root_python_package+".") || module == root_python_package {
			allowed = true
			break
		}
	}
	if !allowed {
		res.cache[module] = &PythonModuleResolverResult{}
		return res.cache[module], nil
	}

	if strings.HasPrefix(module, ".") {
		log.Panicf("Relative imports are not supported: '%s'", module)
	}

	paths := []string{}

	visit_parent := true

	dir_path := strings.ReplaceAll(module, ".", "/")
	dir_path_init := filepath.Join(dir_path, "__init__.py")
	py_path := dir_path + ".py"
	pyx_path := dir_path + ".pyx"
	c_path := dir_path + ".c"
	if _, err := os.Stat(filepath.Join(base_dir, dir_path_init)); err == nil {
		paths = append(paths, dir_path_init)
	} else if _, err := os.Stat(filepath.Join(base_dir, dir_path)); err == nil {
		// This is a namespace package, no file to import
	} else if _, err := os.Stat(filepath.Join(base_dir, py_path)); err == nil {
		paths = append(paths, py_path)
	} else if _, err := os.Stat(filepath.Join(base_dir, pyx_path)); err == nil {
		paths = append(paths, pyx_path)
	} else if _, err := os.Stat(filepath.Join(base_dir, c_path)); err == nil {
		paths = append(paths, c_path)
	} else {
		visit_parent = false
	}

	if visit_parent {
		idx := strings.LastIndex(module, ".")
		if idx != -1 {
			sub_resolve, err := res.Resolve(module[:idx], config, base_dir)
			if err != nil {
				return nil, err
			}
			paths = append(paths, sub_resolve.Paths...)
		}
	}

	out := &PythonModuleResolverResult{
		Paths: paths,
	}
	res.cache[module] = out
	return out, nil
}
