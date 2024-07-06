package main

import (
	"context"
	"crypto/sha256"
	"log"
	"os"
	"path/filepath"
)

// ctx, fileHashes, all_files_set, base_dir
func CalculateFileHashes(
	ctx context.Context,
	fileHashes map[string][32]byte,
	all_files_set map[string]bool,
	base_dir string,
) {
	for file_name := range all_files_set {
		file_path := filepath.Join(base_dir, file_name)
		file_data_bytes, err := os.ReadFile(file_path)
		if err != nil {
			log.Fatalf("Error while reading file '%s': %v", file_path, err)
		}
		fileHashes[file_name] = sha256.Sum256(file_data_bytes)
	}
}
