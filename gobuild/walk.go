package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path"
	"sort"
	"strings"
	"sync"
	"time"

	"dagger/gobuild/internal/dagger"
)

// toolsDir is where package execs that need it find the pack tool, which
// the go command runs in-process.
const toolsDir = "/gobuild/tools"

// node is one exec: a block rendered for the walk that runs it.
type node struct {
	ID         string   `json:"id"`
	Name       string   `json:"name"`
	ImportPath string   `json:"importPath"`
	Deps       []string `json:"deps"`
	SrcFiles   []string `json:"srcFiles,omitempty"`
	ModFiles   []string `json:"modFiles,omitempty"`
	// TierDeps are the dependencies a lower tier built, read from it one
	// archive at a time.
	TierDeps []tierDep `json:"tierDeps,omitempty"`
	// Tiers are the lower tiers the link mounts whole.
	Tiers  []int  `json:"tiers,omitempty"`
	Tools  bool   `json:"tools,omitempty"`
	Output string `json:"output"`
	Script string `json:"script"`
	// Group is std, dep or ws, and DepNames every dependency by name, for
	// the timeline.
	Group    string   `json:"group"`
	DepNames []string `json:"depNames"`
}

type tierDep struct {
	Tier int    `json:"tier"`
	Name string `json:"name"`
}

// toNode renders a block for a walk; outside holds the blocks lower tiers
// built. A package reads each dependency's archive from /work/lib, wherever
// it was built, so its exec is the same under any tier split, and its key
// changes only with the archives it reads, not with a whole tier. The link,
// which reads every archive and re-runs on any change anyway, mounts the
// tiers whole instead of copying each archive.
func toNode(b *block, blocks map[string]*block, outside map[string]int) *node {
	n := &node{ID: b.id, Name: b.name, ImportPath: b.importPath, Deps: b.deps,
		SrcFiles: b.srcFiles, ModFiles: b.modFiles, Output: b.output(), Group: b.group}
	for _, d := range b.deps {
		n.DepNames = append(n.DepNames, blocks[d].name)
	}
	if b.isLink {
		n.Script = b.script(blocks, outside)
		seen := map[int]bool{}
		for _, d := range b.deps {
			if k, ok := outside[d]; ok && !seen[k] {
				seen[k] = true
				n.Tiers = append(n.Tiers, k)
			}
		}
		sort.Ints(n.Tiers)
	} else {
		n.Script = b.script(blocks, nil)
		for _, d := range b.deps {
			if k, ok := outside[d]; ok {
				n.TierDeps = append(n.TierDeps, tierDep{k, blocks[d].name})
			}
		}
	}
	n.Tools = strings.Contains(n.Script, toolsDir+"/")
	return n
}

// walker runs a set of nodes, one exec each, each after its dependencies in
// the set. Dependencies outside the set come from tiers.
type walker struct {
	base  *dagger.Container
	tools *dagger.Directory
	src   *dagger.Directory
	// mods is the module cache, evaluated: see fileTree.
	mods        *dagger.Directory
	roots       roots
	tiers       []*dagger.Directory
	concurrency int
	// timeline prints, per package, when it became ready, started and
	// finished its exec, and how long its inputs took to assemble, synced
	// on their own after it took an exec slot. That adds requests and
	// changes when inputs are evaluated: for diagnosis, not timing.
	timeline bool
}

type result struct {
	file *dagger.File
	err  error // set before done closes; dependents read it
	done chan struct{}

	ready, started, execDone, finished time.Time
	libDur, srcDur, modDur             time.Duration
}

func (w *walker) walk(ctx context.Context, nodes map[string]*node) (map[string]*result, error) {
	results := map[string]*result{}
	for id := range nodes {
		results[id] = &result{done: make(chan struct{})}
	}
	sem := make(chan struct{}, w.concurrency)
	var wg sync.WaitGroup
	for id, n := range nodes {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r := results[id]
			defer close(r.done)
			ctr := w.base
			var depFiles []*dagger.File
			for _, d := range n.Deps {
				dr, ok := results[d]
				if !ok {
					continue
				}
				<-dr.done
				if dr.err != nil {
					r.err = fmt.Errorf("%s: %w", nodes[d].ImportPath, dr.err)
					return
				}
				depFiles = append(depFiles, dr.file)
			}
			for _, td := range n.TierDeps {
				// Read from an evaluated tier, so keyed by its content.
				depFiles = append(depFiles, w.tiers[td.Tier].File(td.Name+".a"))
			}
			var libDir, srcDir, modDir *dagger.Directory
			if len(depFiles) > 0 {
				libDir = w.assemble(depFiles)
				ctr = ctr.WithMountedDirectory("/work/lib", libDir)
			}
			for _, k := range n.Tiers {
				ctr = ctr.WithMountedDirectory(fmt.Sprintf("/work/t%d", k), w.tiers[k],
					dagger.ContainerWithMountedDirectoryOpts{ReadOnly: true})
			}
			if n.Tools {
				ctr = ctr.WithMountedDirectory(toolsDir, w.tools, dagger.ContainerWithMountedDirectoryOpts{ReadOnly: true})
			}
			if len(n.SrcFiles) > 0 {
				var at string
				srcDir, at = selected(w.src, n.SrcFiles)
				ctr = ctr.WithMountedDirectory(path.Join(w.roots.Src, at), srcDir)
			}
			if len(n.ModFiles) > 0 {
				modDir = fileTree(w.mods, n.ModFiles)
				ctr = ctr.WithMountedDirectory(w.roots.Mod, modDir)
			}
			r.ready = time.Now()
			defer func() { r.finished = time.Now() }()
			// A script past the kernel's argument-size limit (the link's
			// import config at a large project's scale) goes in as a file.
			var ran *dagger.Container
			if len(n.Script) > 64<<10 {
				ran = ctr.WithNewFile("/gobuild/run.sh", n.Script).WithExec([]string{"sh", "/gobuild/run.sh"}, noNest)
			} else {
				ran = ctr.WithExec([]string{"sh", "-c", n.Script}, noNest)
			}
			sem <- struct{}{}
			defer func() { <-sem }()
			if w.timeline {
				timed := func(d *dagger.Directory) (time.Duration, error) {
					if d == nil {
						return 0, nil
					}
					t := time.Now()
					_, err := d.Sync(ctx)
					return time.Since(t), err
				}
				var err error
				if r.libDur, err = timed(libDir); err == nil {
					if r.srcDur, err = timed(srcDir); err == nil {
						r.modDur, err = timed(modDir)
					}
				}
				if err != nil {
					r.err = fmt.Errorf("%s: inputs: %w", n.ImportPath, err)
					return
				}
			}
			r.started = time.Now()
			// Evaluate the output directory first, then read the file from
			// it: only a file read from an evaluated directory is keyed by its
			// content, so a package whose archive came out unchanged leaves
			// its dependents' execs cached.
			out, err := ran.Directory("/work/" + n.Name).Sync(ctx)
			if err != nil {
				r.err = err
				return
			}
			r.execDone = time.Now()
			r.file, r.err = out.File(n.Output).Sync(ctx)
		}()
	}
	wg.Wait()
	if w.timeline {
		printTimeline(nodes, results)
	}
	var errs []string
	for _, n := range nodes {
		if r := results[n.ID]; r.err != nil {
			errs = append(errs, r.err.Error())
		}
	}
	if len(errs) > 0 {
		// Report each failure once: the package that failed, not every
		// dependent that waited on it.
		sort.Strings(errs)
		return nil, fmt.Errorf("%s", shortest(errs))
	}
	return results, nil
}

// printTimeline writes one "TL <json>" line per package to stderr, with
// times in Unix milliseconds, so walks in separate calls line up.
func printTimeline(nodes map[string]*node, results map[string]*result) {
	type line struct {
		Pkg      string   `json:"pkg"`
		Name     string   `json:"name"`
		Group    string   `json:"group"`
		Deps     []string `json:"deps"`
		Ready    int64    `json:"ready"`
		Started  int64    `json:"started"`
		ExecDone int64    `json:"execDone"`
		Finished int64    `json:"finished"`
		LibMs    int64    `json:"libMs"`
		SrcMs    int64    `json:"srcMs"`
		ModMs    int64    `json:"modMs"`
	}
	for id, n := range nodes {
		r := results[id]
		pkg := n.ImportPath
		if n.Output != n.Name+".a" {
			pkg += " (link)"
		}
		j, err := json.Marshal(line{pkg, n.Name, n.Group, n.DepNames, r.ready.UnixMilli(), r.started.UnixMilli(),
			r.execDone.UnixMilli(), r.finished.UnixMilli(), r.libDur.Milliseconds(), r.srcDur.Milliseconds(), r.modDur.Milliseconds()})
		if err == nil {
			fmt.Fprintln(os.Stderr, "TL "+string(j))
		}
	}
}

// shortest is the failure at the bottom of a chain: a dependent's error
// wraps its dependency's, so the shortest message is the original.
func shortest(errs []string) string {
	s := errs[0]
	for _, e := range errs[1:] {
		if len(e) < len(s) {
			s = e
		}
	}
	return s
}

// assembleMax is how many separate files one withFiles directory holds. Before
// Dagger v1.0.0-beta.17, withFiles chained one snapshot per file, and past
// roughly 400 the overlay mount's options grew too long for the kernel.
const assembleMax = 300

// assemble returns a directory holding files: one withFiles, or, past
// assembleMax, chunks that one exec copies into a single directory.
func (w *walker) assemble(files []*dagger.File) *dagger.Directory {
	if len(files) <= assembleMax {
		return dag.Directory().WithFiles(".", files)
	}
	ctr := w.base
	var cmd strings.Builder
	cmd.WriteString("set -e; mkdir -p /gobuild/lib")
	for i := 0; i*assembleMax < len(files); i++ {
		end := min((i+1)*assembleMax, len(files))
		dir := fmt.Sprintf("/gobuild/in/%d", i)
		ctr = ctr.WithMountedDirectory(dir, dag.Directory().WithFiles(".", files[i*assembleMax:end]),
			dagger.ContainerWithMountedDirectoryOpts{ReadOnly: true})
		cmd.WriteString("; cp -a " + dir + "/. /gobuild/lib/")
	}
	return ctr.WithExec([]string{"sh", "-c", cmd.String()}, noNest).Directory("/gobuild/lib")
}

// selected is the part of dir that files name, and where under the root to
// mount it: their deepest common directory, filtered to them. A package's
// files share its own directory as a rule, so selecting costs per package,
// not per tree. A pattern such as "**/*.h" selects from the whole tree.
func selected(dir *dagger.Directory, files []string) (*dagger.Directory, string) {
	at := commonDir(files)
	if at == "" {
		return dir.Filter(dagger.DirectoryFilterOpts{Include: files}), ""
	}
	rel := make([]string, len(files))
	for i, f := range files {
		rel[i] = strings.TrimPrefix(f, at+"/")
	}
	return dir.Directory(at).Filter(dagger.DirectoryFilterOpts{Include: rel}), at
}

// fileTree is a directory holding files from dir, the module cache, at the
// same paths. Each file is read on its own from the evaluated cache, so it
// is keyed by its content, and so is the tree. A selection made from the
// cache directory itself would be keyed by the cache's recipe, which a bump
// of any dependency changes, re-running every dependency package.
func fileTree(dir *dagger.Directory, files []string) *dagger.Directory {
	byDir := map[string][]*dagger.File{}
	for _, f := range files {
		byDir[path.Dir(f)] = append(byDir[path.Dir(f)], dir.File(f))
	}
	dirs := make([]string, 0, len(byDir))
	for d := range byDir {
		dirs = append(dirs, d)
	}
	sort.Strings(dirs)
	tree := dag.Directory()
	for _, d := range dirs {
		tree = tree.WithFiles(d, byDir[d])
	}
	return tree
}

// expandHeaders replaces each module header pattern ("<module>/**/*.h", for
// assembly that may include any header in its module) with the headers it
// matches, so every module file a package reads is named.
func expandHeaders(ctx context.Context, mods *dagger.Directory, blocks map[string]*block) error {
	found := map[string][]string{}
	for _, b := range blocks {
		for _, f := range b.modFiles {
			if strings.HasSuffix(f, "/**/*.h") {
				found[f] = nil
			}
		}
	}
	var mu sync.Mutex
	var wg sync.WaitGroup
	var errs []error
	for pattern := range found {
		wg.Add(1)
		go func() {
			defer wg.Done()
			mod := strings.TrimSuffix(pattern, "/**/*.h")
			matches, err := mods.Directory(mod).Glob(ctx, "**/*.h")
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				errs = append(errs, fmt.Errorf("module %s: %w", mod, err))
				return
			}
			for _, m := range matches {
				found[pattern] = append(found[pattern], mod+"/"+m)
			}
		}()
	}
	wg.Wait()
	if len(errs) > 0 {
		return errs[0]
	}
	for _, b := range blocks {
		var files []string
		for _, f := range b.modFiles {
			if hs, ok := found[f]; ok {
				for _, h := range hs {
					files = appendUniq(files, h)
				}
			} else {
				files = appendUniq(files, f)
			}
		}
		sort.Strings(files)
		b.modFiles = files
	}
	return nil
}

func commonDir(files []string) string {
	var common []string
	for i, f := range files {
		parts := strings.Split(f, "/")
		parts = parts[:len(parts)-1]
		for j, p := range parts {
			if strings.ContainsAny(p, "*?[") {
				parts = parts[:j]
				break
			}
		}
		if i == 0 {
			common = parts
			continue
		}
		n := 0
		for n < len(common) && n < len(parts) && common[n] == parts[n] {
			n++
		}
		common = common[:n]
	}
	return strings.Join(common, "/")
}

// partition splits a plan's packages into tiers: sets that separately cached
// calls build, in order, each reading the archives of those before it.
// Packages in no tier are walked by the build's own call. Which split is
// fastest depends on how a project's packages change together and on how
// long a tier makes its dependents wait, so it is chosen by name and kept in
// this one place:
//
//	none     no tiers: the build walks every package.
//	outside  one tier: every package outside the source whose dependencies
//	         are outside too, i.e. the standard library and dependency modules.
//	split    two tiers: the standard library, then the dependency modules.
func partition(strategy string, blocks map[string]*block) ([]map[string]bool, error) {
	switch strategy {
	case "none":
		return nil, nil
	case "outside":
		return nonEmpty(outsideSet(blocks)), nil
	case "split":
		std, outside := stdSet(blocks), outsideSet(blocks)
		for id := range std {
			delete(outside, id)
		}
		return nonEmpty(std, outside), nil
	}
	return nil, fmt.Errorf("unknown tier strategy %q: want none, outside or split", strategy)
}

func nonEmpty(sets ...map[string]bool) []map[string]bool {
	var out []map[string]bool
	for _, s := range sets {
		if len(s) > 0 {
			out = append(out, s)
		}
	}
	return out
}

// closedSet is every block that has property and whose dependencies all
// have it too.
func closedSet(blocks map[string]*block, property func(*block) bool) map[string]bool {
	memo := map[string]bool{}
	var in func(id string) bool
	in = func(id string) bool {
		if v, ok := memo[id]; ok {
			return v
		}
		b := blocks[id]
		v := property(b)
		for _, d := range b.deps {
			if !in(d) {
				v = false
			}
		}
		memo[id] = v
		return v
	}
	set := map[string]bool{}
	for id := range blocks {
		if in(id) {
			set[id] = true
		}
	}
	return set
}

// outsideSet is the packages that read nothing from the source, down to
// their dependencies.
func outsideSet(blocks map[string]*block) map[string]bool {
	return closedSet(blocks, func(b *block) bool { return len(b.srcFiles) == 0 && !b.isLink })
}

// stdSet is the standard library: packages that read neither the source nor
// the module cache, down to their dependencies.
func stdSet(blocks map[string]*block) map[string]bool {
	return closedSet(blocks, func(b *block) bool { return len(b.srcFiles) == 0 && len(b.modFiles) == 0 && !b.isLink })
}

// tierSpec is what Tier needs to build one tier, as stable JSON: nodes
// sorted and keyed by package name rather than plan position, so the same
// tier in another build or plan is the same call.
type tierSpec struct {
	Roots roots   `json:"roots"`
	Nodes []*node `json:"nodes"`
}

func newTierSpec(blocks map[string]*block, tier map[string]bool, outside map[string]int, r roots) (string, *tierSpec, error) {
	spec := &tierSpec{Roots: roots{Mod: r.Mod}}
	for id := range tier {
		n := toNode(blocks[id], blocks, outside)
		deps := []string{}
		for _, d := range n.Deps {
			if tier[d] {
				deps = append(deps, blocks[d].name)
			}
		}
		sort.Strings(deps)
		n.ID, n.Deps = n.Name, deps
		spec.Nodes = append(spec.Nodes, n)
	}
	sort.Slice(spec.Nodes, func(i, j int) bool { return spec.Nodes[i].Name < spec.Nodes[j].Name })
	b, err := json.Marshal(spec)
	return string(b), spec, err
}
