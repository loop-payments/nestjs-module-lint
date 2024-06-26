package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/loop-payments/nestjs-module-lint/internal/parser"
	pathresolver "github.com/loop-payments/nestjs-module-lint/internal/path-resolver"
	sitter "github.com/smacker/go-tree-sitter"
	"github.com/smacker/go-tree-sitter/typescript/typescript"
	mpb "github.com/vbauerster/mpb/v8"
	"github.com/vbauerster/mpb/v8/decor"
)

var AnyOwner = "ANY"
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
	forOwner string,
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
	p := mpb.New(mpb.WithWidth(64))

	bar := p.New(int64(len(files)),
		// BarFillerBuilder with custom style
		mpb.BarStyle(),
		mpb.PrependDecorators(
			// replace ETA decorator with "done" message, OnComplete event
			decor.OnComplete(decor.AverageETA(decor.ET_STYLE_GO), "done"),
		),
		mpb.AppendDecorators(decor.Percentage()),
	)
	var wg sync.WaitGroup
	resultChan := make(chan struct {
		*ModuleReport
		error
	})
	for _, file := range files {
		wg.Add(1)
		go func(file string) {
			defer func() {
				wg.Done()
				bar.Increment()
			}()
			var localWg sync.WaitGroup
			localWg.Add(1)

			var owner string
			var ownerErr error
			// Load OWNERS file
			go func(file string) {
				defer localWg.Done()
				ownersPath, err := FindOwnersFile(filepath.Dir(file))

				if err != nil {
					ownerErr = fmt.Errorf("failed to find owners file for %s: %w", file, err)
					return
				}
				owner, err = ParseOwnersFile(ownersPath)
				if forOwner != AnyOwner && forOwner != owner {
					owner = ""
				}
			}(file)

			moduleReports, err := RunForModuleFile(file)
			if err != nil {
				resultChan <- struct {
					*ModuleReport
					error
				}{nil, fmt.Errorf("failed to run app for %s: %w", file, err)}
			}
			localWg.Wait()
			if ownerErr != nil {
				resultChan <- struct {
					*ModuleReport
					error
				}{nil, ownerErr}
				return
			}
			if owner == "" {
				return
			}

			for _, report := range moduleReports {
				report.Owner = owner
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
		p.Wait()
		close(resultChan)
	}()

	var results []*ModuleReport
	for result := range resultChan {
		if result.error != nil {
			return nil, result.error
		}
		if result.ModuleReport != nil && len(result.ModuleReport.UnnecessaryImports) > 0 {
			results = append(results, result.ModuleReport)
		}
	}

	// sort results by module name
	sort.Slice(results, func(i, j int) bool {
		return results[i].ModuleName < results[j].ModuleName
	})

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
	ModuleName         string   `json:"module_name"`
	Path               string   `json:"path"`
	UnnecessaryImports []string `json:"unnecessary_imports"`
	Owner              string   `json:"owner,omitempty"`
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

func PrettyPrintModuleReport(report *ModuleReport) string {
	builder := strings.Builder{}
	builder.WriteString(fmt.Sprintf("Module: %s\nOwner: %s\nPath: %s\nUnnecessary Imports:\n", report.ModuleName, report.Owner, report.Path))
	for _, imp := range report.UnnecessaryImports {
		builder.WriteString(fmt.Sprintf("\t%s\n", imp))
	}
	return builder.String()
}
