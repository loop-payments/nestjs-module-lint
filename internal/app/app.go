package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"github.com/loop-payments/nestjs-module-lint/internal/parser"
	pathresolver "github.com/loop-payments/nestjs-module-lint/internal/path-resolver"
	sitter "github.com/smacker/go-tree-sitter"
	"github.com/smacker/go-tree-sitter/typescript/typescript"
)

var cwd string
var lang *sitter.Language

func init() {
	_cwd, err := os.Getwd()
	if err != nil {
		panic("Could not get current file path")
	}
	cwd = _cwd
	lang = typescript.GetLanguage()
}

func RunForDirRecursively(
	root string,
) ([]*ModuleReport, error) {
	info, err := os.Stat(root)
	if err != nil {
		return nil, fmt.Errorf("Failed to access path: %w", err)
	}

	var files []string
	if info.IsDir() {
		files, err = FindTSFiles(root)
		if err != nil {
			return nil, fmt.Errorf("Failed to find TypeScript files: %w", err)
		}
	} else {
		files = []string{root}
	}
	var wg sync.WaitGroup
	resultChan := make(chan struct {
		*ModuleReport
		error
	})
	for _, file := range files {
		wg.Add(1)
		go func(file string) {
			defer wg.Done()
			moduleReports, err := RunForModuleFile(file)
			if err != nil {
				resultChan <- struct {
					*ModuleReport
					error
				}{nil, fmt.Errorf("failed to run app for %s: %w", file, err)}
			}
			for _, report := range moduleReports {
				resultChan <- struct {
					*ModuleReport
					error
				}{report, nil}
			}
		}(file)
	}

	// Close the result channel once all goroutines have completed
	go func() {
		wg.Wait()
		close(resultChan)
	}()

	var results []*ModuleReport
	for result := range resultChan {
		if result.error != nil {
			return nil, result.error
		}
		results = append(results, result.ModuleReport)
	}

	return results, nil
}

func RunForModuleFile(
	pathToModule string,
) ([]*ModuleReport, error) {
	qualifiedPathToModule := filepath.Join(cwd, pathToModule)

	pathResolver, err := pathresolver.NewTsPathResolverFromPath(cwd)
	if err != nil {
		return nil, err
	}
	sourceCode, err := os.ReadFile(qualifiedPathToModule)
	if err != nil {
		return nil, errors.Join(errors.New("could not read the input file, does it exist?"), err)
	}
	n, err := sitter.ParseCtx(context.Background(), sourceCode, lang)
	if err != nil {
		return nil, errors.Join(errors.New("could not parse the input file, is it valid typescript?"), err)
	}
	importsByModule, err := parser.GetImportsByModuleFromFile(n, sourceCode)
	if err != nil {
		return nil, err
	}
	fileImports, err := getFileImports(n, sourceCode, pathResolver, qualifiedPathToModule)
	if err != nil {
		return nil, err
	}
	providerControllersByModule, err := parser.GetProviderControllersByModuleFromFile(n, sourceCode)
	if err != nil {
		return nil, err
	}

	moduleReports := make([]*ModuleReport, 0)
	for module, imports := range importsByModule {
		providerControllers, ok := providerControllersByModule[module]
		if !ok {
			moduleReports = append(moduleReports, &ModuleReport{
				ModuleName:         module,
				Path:               qualifiedPathToModule,
				UnnecessaryImports: imports,
			})
			continue
		}

		moduleReport, err := runForModule(module, imports, providerControllers, fileImports, pathResolver, qualifiedPathToModule)
		if err != nil {
			return nil, err
		}
		moduleReports = append(moduleReports, moduleReport)
	}
	return moduleReports, nil
}

type ModuleReport struct {
	ModuleName         string
	Path               string
	UnnecessaryImports []string
}

func runForModule(
	moduleName string,
	importNames []string,
	providerControllers []string,
	fileImports []FileImportNode,
	pathResolver *pathresolver.TsPathResolver,
	qualifiedPathToModule string,
) (*ModuleReport, error) {
	moduleNode := NewModuleNode(moduleName, importNames, providerControllers, fileImports, pathResolver)
	unecessaryInputs, err := moduleNode.Check()
	if err != nil {
		return nil, err
	}
	return &ModuleReport{
		ModuleName:         moduleName,
		Path:               qualifiedPathToModule,
		UnnecessaryImports: unecessaryInputs,
	}, nil
}
