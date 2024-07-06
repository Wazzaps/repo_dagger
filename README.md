# repo_dagger

This tool analyzes a given code repository and generates a file dependency graph for each file. It can then calculate a hash per input file of each of its dependency files.

This allows (for example) to cache test results per file without maintaining an external dependency graph (like Bazel).

## Install

```bash
# Download the latest release
curl -fsSL https://github.com/Wazzaps/repo_dagger/releases/latest/download/repo_dagger-$(uname)-$(uname -m) -o ~/.local/bin/repo_dagger
# Make it executable
chmod +x ~/.local/bin/repo_dagger
```

## Usage

First, you'll need a configuration file. You may use `example_config.yaml` as a starting point. Place it somewhere in your project (if it's not in the root make sure to change `base_dir`).

Now, you may use this command to generate a json file which maps each input file to the hash of all of its dependencies (recursively):

```bash
repo_dagger -config /path/to/repo/repo_dagger.yaml -out-dep-hashes dep_hashes.json
```

If you'd like the raw relations, use this:

```bash
repo_dagger -config /path/to/repo/repo_dagger.yaml -out-relations relations.json
```

If you'd like recursive dependency counts per input file:

```bash
repo_dagger -config /path/to/repo/repo_dagger.yaml -print-dep-stats
```

More interestingly, you can get *reverse* dependency counts (how many files depend on each input file):

```bash
repo_dagger -config /path/to/repo/repo_dagger.yaml -print-rev-dep-stats
```

For more flags run `repo_dagger -h`.

## License

MIT license, see [LICENSE](LICENSE).
