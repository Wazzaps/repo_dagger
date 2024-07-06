package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"runtime/pprof"
	"slices"
	"sort"
	"sync"

	"github.com/bmatcuk/doublestar/v4"
	"github.com/davecgh/go-spew/spew"
	"golang.org/x/sync/semaphore"
)

// This value is bumped any time the program may output different output given the same input
const ALGORITHM_VERSION uint64 = 1
const VERSION = "1.0.0"

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
	OutDepHashes     string
	OutRelations     string
}

func parseArgs() (*Args, error) {
	// Define command line flags
	version := false
	flag.BoolVar(&version, "v", false, "Print version and exit")
	flag.BoolVar(&version, "version", false, "Print version and exit")
	config := flag.String("config", "", "Path to config file")
	verbose := flag.Bool("verbose", false, "Verbose output")
	print_dep_stats := flag.Bool("print-dep-stats", false, "Print forward dependency statistics")
	print_rev_stats := flag.Bool("print-rev-dep-stats", false, "Print reverse dependency statistics")
	stats_sort := flag.String("stats-sort", "count", "Sort statistics by 'count' or 'name'")
	self_profile := flag.Bool("self-profile", false, "Profile the program into 'repo_dagger.prof'")
	out_dep_hashes := flag.String("out-dep-hashes", "", "Output dependency hashes to the specified file")
	out_relations := flag.String("out-relations", "", "Output relations to the specified file")

	// Parse command line args
	flag.Parse()

	if version {
		fmt.Println(VERSION)
		os.Exit(0)
	}

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
		OutDepHashes:     *out_dep_hashes,
		OutRelations:     *out_relations,
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
	config, config_hash, err := LoadConfig(args.Config)
	if err != nil {
		log.Printf("failed to load config file: %v", err)
		return
	}

	if args.Verbose {
		log.Println("Config:")
		spew.Fdump(os.Stderr, config)
	}

	// Iterate over the inputs
	base_dir := filepath.Join(filepath.Dir(args.Config), config.BaseDir)
	log.Println("Base Directory:", base_dir)
	input_files := []string{}
	for _, input := range config.Inputs.items {
		input_files_chunk, err := doublestar.Glob(os.DirFS(base_dir), input)
		if err != nil {
			log.Printf("error while collecting input files: glob '%s': %v", input, err)
			return
		}
		input_files = append(input_files, input_files_chunk...)
	}
	slices.Sort(input_files)
	input_files = slices.Compact(input_files)
	if len(input_files) == 0 {
		log.Println("No input files found. Exiting.")
		return
	}

	// Visit each file recursively, to build the relations map
	all_files_set := map[string]bool{}
	file_relation_map := map[string][]string{}
	log.Println("Generating dependency graph")
	err = VisitRecursively(all_files_set, file_relation_map, input_files, config, args, base_dir)
	if err != nil {
		log.Printf("error while visiting files: %v", err)
		return
	}

	if args.OutRelations != "" {
		// Write as json
		log.Println("Writing relations to:", args.OutRelations)
		f, err := os.Create(args.OutRelations)
		if err != nil {
			log.Printf("error creating out-relations file '%s': %v", args.OutRelations, err)
			return
		}
		defer f.Close()
		enc := json.NewEncoder(f)
		err = enc.Encode(file_relation_map)
		if err != nil {
			log.Printf("error encoding relations: %v", err)
			return
		}
	}

	fileHashes := map[string][32]byte{}
	if args.OutDepHashes != "" {
		log.Println("Calculating file hashes")
		CalculateFileHashes(fileHashes, all_files_set, base_dir)
	}

	type fileStatEntry struct {
		name  string
		count int
	}

	log.Println("Calculating dependency hashes")
	ctx := context.Background()
	maxWorkers := runtime.GOMAXPROCS(0)
	sem := semaphore.NewWeighted(int64(maxWorkers))
	dep_stats_chan := make(chan fileStatEntry, maxWorkers)
	rev_dep_stats := map[string]int{}
	rev_dep_stats_lock := sync.Mutex{}
	dep_hashes := map[string]string{}
	dep_hashes_lock := sync.Mutex{}
	wg := sync.WaitGroup{}
	wg.Add(len(input_files))
	for _, file_name := range input_files {
		go func() {
			sem.Acquire(ctx, 1)
			dep_list := BuildFullDepList(file_relation_map, file_name)
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
			if args.OutDepHashes != "" {
				hasher := sha256.New()

				algo_ver := new(bytes.Buffer)
				binary.Write(algo_ver, binary.LittleEndian, ALGORITHM_VERSION)

				hasher.Write(algo_ver.Bytes())
				hasher.Write(config_hash[:])
				hasher.Write([]byte(file_name))

				for _, dep := range dep_list {
					hasher.Write([]byte(dep))
					dep_hash := fileHashes[dep]
					hasher.Write(dep_hash[:])
				}

				dep_hashes_lock.Lock()
				dep_hashes[file_name] = fmt.Sprintf("%x", hasher.Sum(nil))
				dep_hashes_lock.Unlock()
			}
			sem.Release(1)
			wg.Done()
		}()
	}

	if args.PrintDepStats {
		sorted_stats := make([]fileStatEntry, 0, len(input_files))
		for i := 0; i < len(input_files); i++ {
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
	if args.OutDepHashes != "" {
		// Write as json
		log.Println("Writing dependency hashes to:", args.OutDepHashes)
		f, err := os.Create(args.OutDepHashes)
		if err != nil {
			log.Printf("error creating out-dep-hashes file '%s': %v", args.OutDepHashes, err)
			return
		}
		defer f.Close()
		enc := json.NewEncoder(f)
		err = enc.Encode(dep_hashes)
		if err != nil {
			log.Printf("error encoding dep hashes: %v", err)
			return
		}
	}

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

func BuildFullDepList(file_relation_map map[string][]string, file string) []string {
	visited := map[string]bool{}
	dep_list := []string{}
	var buildDepList func(string)
	buildDepList = func(file string) {
		if visited[file] {
			return
		}
		visited[file] = true
		for _, related_file := range file_relation_map[file] {
			buildDepList(related_file)
		}
		dep_list = append(dep_list, file)
	}
	buildDepList(file)
	slices.Sort(dep_list)
	return dep_list
}
