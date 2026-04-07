// Package export handles exporting parts of the repo to other directories.
// This is useful if, for example, one wanted to separate out part of
// their repo with all dependencies.
package export

import (
	"bytes"
	"fmt"
	iofs "io/fs"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	"github.com/thought-machine/please/src/cli/logging"
	"github.com/thought-machine/please/src/core"
	"github.com/thought-machine/please/src/fs"
	"github.com/thought-machine/please/src/parse"
)

var log = logging.Log

type export struct {
	state     *core.BuildState
	targetDir string
	noTrim    bool

	exportedTargets    map[core.BuildLabel]bool
	exportedPackages   map[string]bool
	selectedStatements map[string]map[core.BuildStatement]bool
}

// ToDir exports a set of targets to the given directory.
// It dies on any errors.
func ToDir(state *core.BuildState, dir string, noTrim bool, targets []core.BuildLabel) {
	e := &export{
		state:              state,
		noTrim:             noTrim,
		targetDir:          dir,
		exportedPackages:   map[string]bool{},
		exportedTargets:    map[core.BuildLabel]bool{},
		selectedStatements: map[string]map[core.BuildStatement]bool{},
	}

	if err := os.MkdirAll(dir, fs.DirPermissions); err != nil {
		log.Fatalf("failed to create export directory %s: %v", dir, err)
	}

	e.exportPlzConf()
	for _, target := range state.Config.Parse.PreloadSubincludes {
		for _, includeLabel := range append(state.Graph.TransitiveSubincludes(target), target) {
			e.export(state.Graph.TargetOrDie(includeLabel))
		}
	}

	log.Warningf("Exporting selected targets: %v", targets)
	for _, target := range targets {
		e.export(state.Graph.TargetOrDie(target))
	}


	// Write any preloaded build defs as well; preloaded subincludes should be fine though.
	for _, preload := range state.Config.Parse.PreloadBuildDefs {
		if err := fs.RecursiveCopy(preload, filepath.Join(dir, preload), 0); err != nil {
			log.Fatalf("Failed to copy preloaded build def %s: %s", preload, err)
		}
	}
	e.writeBuildStatements()
}

func (e *export) exportPlzConf() {
	profiles, err := filepath.Glob(".plzconfig*")
	if err != nil {
		log.Fatalf("failed to glob .plzconfig files: %v", err)
	}
	for _, file := range profiles {
		path := filepath.Join(e.targetDir, file)
		if err := os.RemoveAll(path); err != nil {
			log.Fatalf("failed to remove .plzconfig file %s: %v", file, err)
		}
		if err := fs.CopyFile(file, path, 0); err != nil {
			log.Fatalf("failed to copy .plzconfig file %s: %v", file, err)
		}
	}
}

// exportSources exports any source files (srcs and data) for the rule
func (e *export) exportSources(target *core.BuildTarget) {
	for _, src := range append(target.AllSources(), target.AllData()...) {
		if _, ok := src.Label(); !ok { // We'll handle these dependencies later
			for _, p := range src.FullPaths(e.state.Graph) {
				if !filepath.IsAbs(p) { // Don't copy system file deps.
					if err := fs.RecursiveCopy(p, filepath.Join(e.targetDir, p), 0); err != nil {
						log.Fatalf("Error copying file: %s\n", err)
					}
					log.Warning("Writing source file: %s", p)
				}
			}
		}
	}
}

var ignoreDirectories = map[string]bool{
	"plz-out": true,
	".git":    true,
	".svn":    true,
	".hg":     true,
}

// exportEntirePackage exports the package BUILD file containing the given target and all sources
func (e *export) exportEntirePackage(target *core.BuildTarget) {
	pkgName := target.Label.PackageName
	if pkgName == parse.InternalPackageName {
		return
	}
	if e.exportedPackages[pkgName] {
		return
	}
	e.exportedPackages[pkgName] = true

	pkgDir := filepath.Clean(pkgName)

	err := filepath.WalkDir(pkgDir, func(path string, d iofs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if path != pkgDir && fs.IsPackage(e.state.Config.Parse.BuildFileName, path) {
				return filepath.SkipDir // We want to stop when we find another package in our dir tree
			}
			if ignoreDirectories[d.Name()] {
				return filepath.SkipDir
			}
			return nil
		}
		if !d.Type().IsRegular() {
			return nil // Ignore symlinks, which are almost certainly generated sources.
		}
		dest := filepath.Join(e.targetDir, path)
		if err := fs.EnsureDir(dest); err != nil {
			return err
		}
		return fs.CopyFile(path, dest, 0)
	})
	if err != nil {
		log.Fatalf("failed to export package %s for %s: %v", pkgName, target.Label, err)
	}
}

// selectBuildStatements exports BUILD statements that generate the build target.
func (e *export) selectBuildStatements(target *core.BuildTarget) {
	if target.Label.PackageName == parse.InternalPackageName {
		return
	}

	log.Infof("Selecting Build stmts of %s with callstack:\n", target.Label.String())
	for _, frame := range target.ParseMetadata.CallStack {
		log.Infof("\t%v", frame)

		if frame.Statement.Filename != "" && !strings.HasPrefix(frame.Statement.Filename, core.OutDir) {
			if _, ok := e.selectedStatements[frame.Statement.Filename]; !ok {
				e.selectedStatements[frame.Statement.Filename] = map[core.BuildStatement]bool{}
			}
			e.selectedStatements[frame.Statement.Filename][frame.Statement] = true
		}
		if frame.Statement.Filename == "" {
			log.Warning("Package without filename %v", frame)
		}
	}
}

// export implements the logic of ToDir, but prevents repeating targets.
func (e *export) export(target *core.BuildTarget) {
	if e.exportedTargets[target.Label] {
		return
	}
	log.Warningf("Exporting %v.\n", target.Label)
	e.exportedTargets[target.Label] = true

	// We want to export the package that made this subrepo available, but we still need to walk the target deps
	// as it may depend on other subrepos or first party targets
	if target.Subrepo != nil && target.Subrepo.Target != nil {
		log.Warningf("Subrepo: %v", target.Subrepo.Target)
		e.export(target.Subrepo.Target)
	} else if e.noTrim {
		// Export the whole package, rather than trying to trim the package down to only the targets we need
		e.exportEntirePackage(target)
	} else {
		e.selectBuildStatements(target)
		e.exportSources(target)
	}

	for _, dep := range target.Dependencies() {
		e.export(dep)
	}
	for _, subinclude := range e.state.Graph.PackageOrDie(target.Label).AllSubincludes(e.state.Graph) {
		e.export(e.state.Graph.TargetOrDie(subinclude))
	}

	for _, otherTarget := range target.RelatedTargets(e.state.Graph) {
		log.Warningf("Exporting Other %s", otherTarget)
		e.export(otherTarget)
	}
}

func printMap(m map[string]map[core.BuildStatement]bool) {
	fmt.Println("PrintMap")
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		fmt.Println(k, m[k])
	}
}

// writeBuildStatements writes the BUILD file statements to the export directory.
func (e *export) writeBuildStatements() {
	log.Warningf("Selected Statements: %v", e.selectedStatements)
	printMap(e.selectedStatements)

	for filename, stmtMap := range e.selectedStatements {
		stmts := make([]core.BuildStatement, 0, len(stmtMap))
		for stmt := range stmtMap {
			stmts = append(stmts, stmt)
		}
		// Sort statements by position to keep them in order
		slices.SortFunc(stmts, func(a, b core.BuildStatement) int {
			return a.Start - b.Start
		})

		e.writeBuildFile(filename, stmts)
	}
}

func (e *export) writeBuildFile(filename string, stmts []core.BuildStatement) {
	log.Debugf("Writing file: %s", filename)

	fr, err := os.Open(filename)
	if err != nil {
		log.Fatalf("failed to open file original file: %v", err)
		return
	}
	defer fr.Close()

	frStat, err := fr.Stat()
	if err != nil {
		log.Fatalf("failed to get original file status: %v", err)
	}

	outputFile := filepath.Join(e.targetDir, filename)
	if err := fs.EnsureDir(outputFile); err != nil {
		log.Fatalf("failed to create output directory structure: %v", err)
	}

	stmtsContent := make([][]byte, 0, len(stmts))
	for _, s := range stmts {
		buff := make([]byte, s.Len())
		_, err := fr.ReadAt(buff, int64(s.Start))
		if err != nil {
			log.Fatalf("failed to read original BUILD file %s: %v", filename, err)
		}
		stmtsContent = append(stmtsContent, buff)
	}

	joined := bytes.Join(stmtsContent, []byte{'\n'})
	joined = append(joined, '\n')
	if err = os.WriteFile(outputFile, joined, frStat.Mode()); err != nil {
		log.Fatalf("failed to write exported BUILD file to %s: %v", filename, err)
	}
}

// Outputs exports the outputs of a target.
func Outputs(state *core.BuildState, dir string, targets []core.BuildLabel) {
	for _, label := range targets {
		target := state.Graph.TargetOrDie(label)
		for _, out := range target.Outputs() {
			fullPath := filepath.Join(dir, out)
			outDir := filepath.Dir(fullPath)
			if err := os.MkdirAll(outDir, core.DirPermissions); err != nil {
				log.Fatalf("Failed to create export dir %s: %s", outDir, err)
			}
			if err := fs.RecursiveCopy(filepath.Join(target.OutDir(), out), fullPath, target.OutMode()|0200); err != nil {
				log.Fatalf("Failed to copy export file: %s", err)
			}
		}
	}
}
