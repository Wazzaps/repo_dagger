package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"runtime/pprof"
	"slices"
	"sort"
	"strings"
	"sync"

	"github.com/bmatcuk/doublestar/v4"
	"github.com/davecgh/go-spew/spew"
	"golang.org/x/sync/semaphore"
)

type StatsSortVal int

const STATS_SORT_COUNT StatsSortVal = 0
const STATS_SORT_NAME StatsSortVal = 1

func StatsSortValFromString(val string) (StatsSortVal, error) {
	switch val {
	case "count":
		return STATS_SORT_COUNT, nil
	case "name":
		return STATS_SORT_NAME, nil
	default:
		return 0, fmt.Errorf("invalid stats-sort value: %s", val)
	}
}

type Args struct {
	Config           string
	Verbose          bool
	PrintDepStats    bool
	PrintRevDepStats bool
	StatsSort        StatsSortVal
	SelfProfile      bool
}

func parseArgs() (*Args, error) {
	// Define command line flags
	config := flag.String("config", "", "Path to config file")
	verbose := flag.Bool("verbose", false, "Verbose output")
	print_dep_stats := flag.Bool("print-dep-stats", false, "Print forward dependency statistics")
	print_rev_stats := flag.Bool("print-rev-dep-stats", false, "Print reverse dependency statistics")
	stats_sort := flag.String("stats-sort", "count", "Sort statistics by 'count' or 'name'")
	self_profile := flag.Bool("self-profile", false, "Profile the program into 'repo_dagger.prof'")

	// Parse command line args
	flag.Parse()

	// Validate the parsed flag values
	if *config == "" {
		return nil, fmt.Errorf("config path not specified")
	}
	stats_sort_val, err := StatsSortValFromString(*stats_sort)
	if err != nil {
		return nil, err
	}

	return &Args{
		Config:           *config,
		Verbose:          *verbose,
		PrintDepStats:    *print_dep_stats,
		PrintRevDepStats: *print_rev_stats,
		StatsSort:        stats_sort_val,
		SelfProfile:      *self_profile,
	}, nil
}

func main() {
	log.SetFlags(log.Ltime | log.Lmicroseconds)
	args, err := parseArgs()
	if err != nil {
		flag.Usage()
		log.Println("Error:", err)
		return
	}

	if args.SelfProfile {
		f, err := os.Create("repo_dagger.prof")
		if err != nil {
			log.Fatal(err)
		}
		pprof.StartCPUProfile(f)
		defer pprof.StopCPUProfile()
	}

	log.Println("Loading Config:", args.Config)

	// Load the config file
	config, err := LoadConfig(args.Config)
	if err != nil {
		log.Printf("Error: %v", err)
		return
	}

	if args.Verbose {
		log.Println("Config:")
		spew.Fdump(os.Stderr, config)
	}
	// Iterate over the inputs
	base_dir := filepath.Join(filepath.Dir(args.Config), config.Basedir)
	log.Println("Base Directory:", base_dir)
	input_files := []string{}
	for _, input := range config.Inputs.items {
		input_files_chunk, err := doublestar.Glob(os.DirFS(base_dir), input)
		if err != nil {
			log.Printf("Error: %v", err)
			return
		}
		input_files = append(input_files, input_files_chunk...)
	}
	slices.Sort(input_files)
	input_files = slices.Compact(input_files)
	// spew.Fdump(os.Stderr, input_files)
	if len(input_files) == 0 {
		log.Println("No input files found. Exiting.")
		return
	}

	// Visit each file
	file_slices_wg := sync.WaitGroup{}
	// TODO: Split the queue by partition, using hash.fnv.New32a
	queue := make(chan []string, 1)

	file_slices_wg.Add(1)
	queue <- input_files

	go func() {
		file_slices_wg.Wait()
		close(queue)
	}()

	python_module_resolver := NewPythonModuleResolver()

	file_related_map := map[string][]string{}
	VisitorWorker(
		file_related_map, queue, &file_slices_wg, python_module_resolver, config, args, base_dir,
	)

	ctx := context.Background()
	maxWorkers := runtime.GOMAXPROCS(0)
	sem := semaphore.NewWeighted(int64(maxWorkers))
	wg := sync.WaitGroup{}
	wg.Add(len(file_related_map))

	type fileStatEntry struct {
		name  string
		count int
	}
	dep_stats_chan := make(chan fileStatEntry, maxWorkers)
	rev_dep_stats := map[string]int{}
	rev_dep_stats_lock := sync.Mutex{}
	for file_name := range file_related_map {
		go func() {
			sem.Acquire(ctx, 1)
			dep_list := BuildFullDepList(file_related_map, file_name)
			if args.PrintDepStats {
				dep_stats_chan <- fileStatEntry{
					name:  file_name,
					count: len(dep_list),
				}
			}
			if args.PrintRevDepStats {
				rev_dep_stats_lock.Lock()
				for _, dep := range dep_list {
					rev_dep_stats[dep]++
				}
				rev_dep_stats_lock.Unlock()
			}
			sem.Release(1)
			wg.Done()
		}()
	}

	if args.PrintDepStats {
		sorted_stats := make([]fileStatEntry, 0, len(file_related_map))
		for i := 0; i < len(file_related_map); i++ {
			sorted_stats = append(sorted_stats, <-dep_stats_chan)
		}
		sort.Slice(sorted_stats, func(i, j int) bool {
			if args.StatsSort == STATS_SORT_COUNT {
				return sorted_stats[i].count > sorted_stats[j].count
			} else if args.StatsSort == STATS_SORT_NAME {
				return sorted_stats[i].name < sorted_stats[j].name
			} else {
				log.Panicf("Invalid stats sort value: %d", args.StatsSort)
				return false
			}
		})
		for _, stat := range sorted_stats {
			log.Printf("%d\t%s", stat.count, stat.name)
		}
	}
	wg.Wait()
	log.Println("Full Graph building done")
	if args.PrintRevDepStats {
		rev_dep_stats_sorted := make([]string, 0, len(rev_dep_stats))
		for k := range rev_dep_stats {
			rev_dep_stats_sorted = append(rev_dep_stats_sorted, k)
		}
		sort.Slice(rev_dep_stats_sorted, func(i, j int) bool {
			if args.StatsSort == STATS_SORT_COUNT {
				a := rev_dep_stats[rev_dep_stats_sorted[i]]
				b := rev_dep_stats[rev_dep_stats_sorted[j]]
				if a == b {
					return rev_dep_stats_sorted[i] < rev_dep_stats_sorted[j]
				} else {
					return a > b
				}
			} else if args.StatsSort == STATS_SORT_NAME {
				return rev_dep_stats_sorted[i] < rev_dep_stats_sorted[j]
			} else {
				log.Panicf("Invalid stats sort value: %d", args.StatsSort)
				return false
			}
		})
		for _, stat := range rev_dep_stats_sorted {
			log.Printf("%d\t%s", rev_dep_stats[stat], stat)
		}

	}

	log.Println("Done")
}

var python_import_parser_simple = regexp.MustCompile(`(?m:^ *import ([^ \n]+))`)
var python_import_parser_from = regexp.MustCompile(
	`(?m:^ *from ([^ \n]+) import (\([^)]+\)|[^\n]+))`,
)
var python_import_parser_ident = regexp.MustCompile(`[A-Za-z_][A-Za-z0-9_]*`)

type PythonModuleResolverResult struct {
	Paths []string
}

type PythonModuleResolver struct {
	cache map[string]*PythonModuleResolverResult
}

func NewPythonModuleResolver() *PythonModuleResolver {
	return &PythonModuleResolver{
		cache: map[string]*PythonModuleResolverResult{},
	}
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

func BuildFullDepList(file_related_map map[string][]string, file string) []string {
	visited := map[string]bool{}
	dep_list := []string{}
	var buildDepList func(string)
	buildDepList = func(file string) {
		if visited[file] {
			return
		}
		visited[file] = true
		for _, related_file := range file_related_map[file] {
			buildDepList(related_file)
		}
		dep_list = append(dep_list, file)
	}
	buildDepList(file)
	slices.Sort(dep_list)
	return dep_list
}

type RegexResult []string

func (res RegexResult) ApplyOnTemplate(template string) string {
	out := template
	for i, match := range res {
		out = strings.ReplaceAll(out, fmt.Sprintf("$%d", i), match)
	}
	return out
}

func (res RegexResult) ApplyOnTemplates(templates []string) (out []string) {
	for _, template := range templates {
		out = append(out, res.ApplyOnTemplate(template))
	}
	return
}

func ApplyActions(
	actions *RuleActions,
	file string,
	file_data **string,
	file_related_files *[]string,
	python_mod_resolver *PythonModuleResolver,
	config *Config,
	args *Args,
	base_dir string,
	regex_result RegexResult,
) error {
	// Visit files
	for _, visit := range regex_result.ApplyOnTemplates(actions.Visit.items) {
		visit_files_chunk, err := doublestar.Glob(os.DirFS(base_dir), visit)
		if err != nil {
			return fmt.Errorf("error while visiting '%s': %v", visit, err)
		}
		*file_related_files = append(*file_related_files, visit_files_chunk...)
	}

	// Visit siblings
	path_iter := filepath.Dir(file)
	for _, visit := range regex_result.ApplyOnTemplates(actions.VisitSiblings.items) {
		visit_files_chunk, err := doublestar.Glob(
			os.DirFS(filepath.Join(base_dir, path_iter)),
			visit,
		)
		if err != nil {
			return fmt.Errorf("error while visiting sibling '%s': %v", visit, err)
		}
		for _, visit_file := range visit_files_chunk {
			*file_related_files = append(*file_related_files, filepath.Join(path_iter, visit_file))
		}
	}

	// Visit grand siblings
	for path_iter != "." {
		for _, visit := range regex_result.ApplyOnTemplates(actions.VisitGrandSiblings.items) {
			visit_files_chunk, err := doublestar.Glob(
				os.DirFS(filepath.Join(base_dir, path_iter)),
				visit,
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
				*file_related_files = append(
					*file_related_files,
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
			for _, mod_name := range regex_result.ApplyOnTemplates(
				actions.VisitPythonAllSubmodulesFor.items,
			) {
				full_mod_name, ok := pyimports_idents[mod_name]
				if !ok {
					return fmt.Errorf("module ident '%s' not found", mod_name)
				}
				if args.Verbose {
					log.Println("Visiting all submodules of:", mod_name, "->", full_mod_name)
				}
				dir_path := strings.ReplaceAll(full_mod_name, ".", "/")

				visit_files_chunk, err := doublestar.Glob(os.DirFS(base_dir), dir_path+"/**/*.py")
				if err != nil {
					return fmt.Errorf("error while visiting submodule '%s': %v", full_mod_name, err)
				}
				*file_related_files = append(*file_related_files, visit_files_chunk...)
			}
		}

		// Resolve the imports
		for _, module := range pyimports {
			paths, err := python_mod_resolver.Resolve(module, config, base_dir)
			if err != nil {
				return fmt.Errorf("error while resolving python module '%s': %v", module, err)
			}
			*file_related_files = append(*file_related_files, paths.Paths...)
		}
	}

	return nil
}

func CheckExcludePatterns(exclude_patterns []string, file string) (bool, error) {
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

func VisitFile(
	file string,
	file_related_files *[]string,
	python_mod_resolver *PythonModuleResolver,
	regex_cache map[string]*regexp.Regexp,
	config *Config,
	args *Args,
	base_dir string,
) error {
	// Ignore globally excluded files
	excluded, err := CheckExcludePatterns(config.GlobalExclude.items, file)
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

			err := ApplyActions(
				&path_rules.Actions,
				file,
				&file_data,
				file_related_files,
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
				excluded, err := CheckExcludePatterns(regex_actions.Exclude.items, file)
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
					err := ApplyActions(
						&regex_actions,
						file,
						&file_data,
						file_related_files,
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

func VisitorWorker(
	file_related_map map[string][]string,
	queue chan []string,
	file_slices_wg *sync.WaitGroup,
	python_mod_resolver *PythonModuleResolver,
	config *Config,
	args *Args,
	base_dir string,
) {
	visited := map[string]bool{}
	regex_cache := map[string]*regexp.Regexp{}

	// Visit each slice in the queue
	for files_slice := range queue {
		slice_related_files := []string{}
		if args.Verbose {
			log.Println("---")
		}
		// Visit each file in the slice
		for _, file := range files_slice {
			if visited[file] {
				continue
			}
			visited[file] = true
			file_related_files := config.GlobalDeps.items

			err := VisitFile(file, &file_related_files, python_mod_resolver, regex_cache, config, args, base_dir)
			if err != nil {
				log.Printf("error while visiting file '%s': %v", file, err)
				continue
			}

			// Sort, dedup, and save the related files
			slices.Sort(file_related_files)
			file_related_files = slices.Compact(file_related_files)
			file_related_map[file] = file_related_files
			slice_related_files = append(slice_related_files, file_related_files...)
		}

		if len(slice_related_files) != 0 {
			// Sort, dedup, and send the slice to the queue
			slices.Sort(slice_related_files)
			file_slices_wg.Add(1)
			queue <- slices.Compact(slice_related_files)
		}
		file_slices_wg.Done()
	}
}
