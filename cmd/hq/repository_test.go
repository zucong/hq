package main

import "path/filepath"

// repositoryPath keeps tests independent of the package working directory
// while repository-level documents and examples live outside cmd/hq.
func repositoryPath(parts ...string) string {
	return filepath.Join(append([]string{"..", ".."}, parts...)...)
}
