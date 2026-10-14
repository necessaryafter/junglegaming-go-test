package app_test

import (
	"go/parser"
	gotoken "go/token"
	"io/fs"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// TestModuleBoundaries makes the dependency direction an enforced contract.
// Integration tests can inspect adapters; production code must use module APIs.
func TestModuleBoundaries(t *testing.T) {
	const domain = "jungle/internal/wagering/domain/"
	const platform = "jungle/internal/platform/"
	const wagering = "jungle/internal/wagering/"

	err := filepath.WalkDir("..", func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}

		if entry.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}

		relative, err := filepath.Rel("..", path)
		if err != nil {
			return err
		}

		relative = filepath.ToSlash(relative)
		file, err := parser.ParseFile(gotoken.NewFileSet(), path, nil, parser.ImportsOnly)
		if err != nil {
			return err
		}

		isPlatformPackage := strings.HasPrefix(relative, "platform/")
		isWageringPackage := strings.HasPrefix(relative, "wagering/")
		isDomainPackage := strings.HasPrefix(relative, "wagering/domain/")

		for _, spec := range file.Imports {
			dependency, err := strconv.Unquote(spec.Path.Value)
			if err != nil {
				return err
			}

			isLocalDependency := strings.HasPrefix(dependency, "jungle/")
			isPlatformDependency := strings.HasPrefix(dependency, platform)

			if isPlatformPackage && isLocalDependency && !isPlatformDependency {
				t.Errorf("platform must not depend on business modules: %s imports %s", relative, dependency)
			}

			if !isWageringPackage && strings.HasPrefix(dependency, wagering) {
				t.Errorf("external packages must use the wagering module's public API: %s imports %s", relative, dependency)
			}

			if !isDomainPackage {
				continue
			}

			localOutsideDomain := isLocalDependency && !strings.HasPrefix(dependency, domain)
			externalLibrary := strings.Contains(strings.Split(dependency, "/")[0], ".")

			if localOutsideDomain || externalLibrary {
				t.Errorf("domain must depend only on itself and the standard library: %s imports %s", relative, dependency)
			}
		}

		return nil
	})
	if err != nil {
		t.Fatalf("inspect module boundaries: %v", err)
	}
}
