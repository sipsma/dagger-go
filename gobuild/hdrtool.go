package main

import "dagger/gobuild/internal/dagger"

// hdrTool builds gohdr, the helper's in-container tool. "gohdr unpack" writes
// out a header pack, the go module's scan of each Go file up to the end of its
// imports, for the plan. "gohdr weigh" weighs the plan's steps by the Go
// source they compile. "gohdr cc" answers the plan's C compiler probes.
// "gohdr run" builds a tier: it runs the plan's per-package scripts in one
// container, in dependency order. "gohdr linkid" makes a link's build ID.
func hdrTool() *dagger.File {
	// A standalone tool, so it is built with a pinned toolchain, statically,
	// and runs in whatever image the module builds with.
	return dag.Container().From(toolImage).
		WithEnvVariable("CGO_ENABLED", "0").
		WithNewFile("/gohdr-src/go.mod", "module gohdr\n\ngo 1.26\n").
		WithNewFile("/gohdr-src/main.go", hdrToolSrc).
		WithWorkdir("/gohdr-src").
		WithExec([]string{"go", "build", "-o", "/gohdr", "."}, noNest).File("/gohdr")
}

const hdrToolSrc = `package main

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

func unpack(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	r := bufio.NewReader(in)
	line := func() (string, error) {
		s, err := r.ReadString('\n')
		return strings.TrimSuffix(s, "\n"), err
	}
	if magic, err := line(); err != nil || magic != "gohdr 1" {
		return fmt.Errorf("not a gohdr pack")
	}
	for {
		rel, err := line()
		if err == io.EOF && rel == "" {
			return nil
		}
		if err != nil {
			return err
		}
		modeStr, err := line()
		if err != nil {
			return err
		}
		sizeStr, err := line()
		if err != nil {
			return err
		}
		mode, err := strconv.ParseUint(modeStr, 8, 32)
		if err != nil {
			return err
		}
		size, err := strconv.Atoi(sizeStr)
		if err != nil {
			return err
		}
		data := make([]byte, size+1)
		if _, err := io.ReadFull(r, data); err != nil {
			return err
		}
		p := filepath.Join(dst, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			return err
		}
		if fs.FileMode(mode)&fs.ModeSymlink != 0 {
			if err := os.Symlink(string(data[:size]), p); err != nil {
				return err
			}
			continue
		}
		if err := os.WriteFile(p, data[:size], fs.FileMode(mode)); err != nil {
			return err
		}
	}
}

type runNode struct {
	Name   string
	Pkg    string
	Deps   []string
	Output string
	Script string
	Height int64
}

type runSpec struct {
	Lib   string
	Nodes []runNode
}

// run builds every node of the spec in this container: each script once the
// nodes it depends on are done, as many at once as there are CPUs, the
// highest first: the spec's height is the heaviest chain of packages that
// import the node, in this tier or a later one, weighed as weigh does. Each
// output is linked into the lib directory, where dependents read it.
//
// Measurement build: it also writes traceFile, one JSON line per node with
// when it became ready, started and ended (wall clock, Unix nanoseconds),
// which node's end made it ready, and its script's CPU time, then a last
// line for the whole run.
func run(specPath string) error {
	runStart := time.Now()
	data, err := os.ReadFile(specPath)
	if err != nil {
		return err
	}
	var spec runSpec
	if err := json.Unmarshal(data, &spec); err != nil {
		return err
	}
	if err := os.MkdirAll(spec.Lib, 0o755); err != nil {
		return err
	}
	nodes := map[string]*runNode{}
	for i := range spec.Nodes {
		nodes[spec.Nodes[i].Name] = &spec.Nodes[i]
	}
	dependents := map[string][]string{}
	pending := map[string]int{}
	for _, n := range spec.Nodes {
		for _, d := range n.Deps {
			if _, ok := nodes[d]; ok {
				dependents[d] = append(dependents[d], n.Name)
				pending[n.Name]++
			}
		}
	}
	type result struct {
		name string
		out  []byte
		err  error
		tr   nodeTrace
	}
	results := make(chan result)
	traces := map[string]*nodeTrace{}
	var ready []string
	for name := range nodes {
		if pending[name] == 0 {
			ready = append(ready, name)
			traces[name] = &nodeTrace{Ready: runStart.UnixNano()}
		}
	}
	jobs := runtime.NumCPU()
	running, finished := 0, 0
	for finished < len(nodes) {
		sort.Slice(ready, func(i, j int) bool {
			if hi, hj := nodes[ready[i]].Height, nodes[ready[j]].Height; hi != hj {
				return hi > hj
			}
			return ready[i] < ready[j]
		})
		for running < jobs && len(ready) > 0 {
			n := nodes[ready[0]]
			ready = ready[1:]
			running++
			traces[n.Name].Dispatched = time.Now().UnixNano()
			traces[n.Name].Outstanding = running
			go func() {
				out, tr, err := runOne(n, spec.Lib)
				results <- result{n.Name, out, err, tr}
			}()
		}
		r := <-results
		running--
		finished++
		n := nodes[r.name]
		t := traces[r.name]
		t.Start, t.End, t.Linked = r.tr.Start, r.tr.End, r.tr.Linked
		t.UserUS, t.SysUS, t.MaxRSSKB = r.tr.UserUS, r.tr.SysUS, r.tr.MaxRSSKB
		if r.err != nil {
			os.Stderr.Write(r.out)
			return fmt.Errorf("%s: %v", n.Pkg, r.err)
		}
		for _, up := range dependents[r.name] {
			if pending[up]--; pending[up] == 0 {
				ready = append(ready, up)
				traces[up] = &nodeTrace{Ready: time.Now().UnixNano(), ReadyAfter: r.name}
			}
		}
	}
	return writeTrace(spec, traces, runStart, jobs)
}

// traceFile is where run writes its trace: outside the lib directory, which
// dependents mount.
const traceFile = "/work/gohdr-trace.jsonl"

// nodeTrace is what run records for a node. Times are Unix nanoseconds.
type nodeTrace struct {
	Ready int64
	// ReadyAfter is the in-tier dependency whose result made the node
	// ready, as the scheduler received it (after its link into lib); empty
	// when it was ready from the start.
	ReadyAfter string
	// Dispatched is when the scheduler gave the node a slot; Outstanding is
	// how many nodes held a slot then, this one included.
	Dispatched  int64
	Outstanding int
	// Start is just before the script's command started, after its file
	// was written; End is when the command's wait returned; Linked is when
	// its output was in lib.
	Start, End, Linked int64
	// The script's CPU time, its waited-for children included, and the
	// largest single process's peak RSS.
	UserUS, SysUS int64
	MaxRSSKB      int64
}

func writeTrace(spec runSpec, traces map[string]*nodeTrace, runStart time.Time, jobs int) error {
	reConc := regexp.MustCompile("\\s-c=(\\d+)")
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	for _, n := range spec.Nodes {
		t := traces[n.Name]
		c := 0
		if m := reConc.FindStringSubmatch(n.Script); m != nil {
			c, _ = strconv.Atoi(m[1])
		}
		if err := enc.Encode(map[string]any{
			"name": n.Name, "pkg": n.Pkg, "deps": n.Deps, "height": n.Height, "c": c,
			"ready": t.Ready, "ready_after": t.ReadyAfter, "start": t.Start, "end": t.End,
			"linked": t.Linked, "dispatched": t.Dispatched, "outstanding": t.Outstanding,
			"user_us": t.UserUS, "sys_us": t.SysUS,
			"maxrss_kb": t.MaxRSSKB,
		}); err != nil {
			return err
		}
	}
	if err := enc.Encode(map[string]any{
		"run": true, "start": runStart.UnixNano(), "end": time.Now().UnixNano(),
		"jobs": jobs, "nodes": len(spec.Nodes),
	}); err != nil {
		return err
	}
	return os.WriteFile(traceFile, b.Bytes(), 0o644)
}

// weigh prints a line "<step> <weight>" for each step (bNNN) of a go build
// -n plan: the bytes of Go source the step's lines name, plus a fixed cost
// for starting a compile, a stand-in for its compile time. A relative file is
// in the directory the plan last changed to, from the current one, as the go
// command prints "cd" only when the directory changes.
func weigh(planPath string) error {
	data, err := os.ReadFile(planPath)
	if err != nil {
		return err
	}
	start, err := os.Getwd()
	if err != nil {
		return err
	}
	dir := start
	weights := map[string]int64{}
	var steps []string
	step := ""
	for _, line := range strings.Split(string(data), "\n") {
		if rest, ok := strings.CutPrefix(line, "mkdir -p $WORK/b"); ok {
			if n, _, ok := strings.Cut(rest, "/"); ok && n != "" && strings.Trim(n, "0123456789") == "" {
				step = "b" + n
				if _, ok := weights[step]; !ok {
					weights[step] = 8 << 10
					steps = append(steps, step)
				}
			}
		}
		if d, ok := strings.CutPrefix(line, "cd "); ok {
			dir = d
			if d == "." {
				dir = start
			}
			continue
		}
		if step == "" {
			continue
		}
		for _, f := range strings.Fields(line) {
			f = strings.Trim(f, "\"'")
			if !strings.HasSuffix(f, ".go") {
				continue
			}
			if !filepath.IsAbs(f) {
				f = filepath.Join(dir, f)
			}
			if fi, err := os.Stat(f); err == nil {
				weights[step] += fi.Size()
			}
		}
	}
	w := bufio.NewWriter(os.Stdout)
	for _, step := range steps {
		fmt.Fprintf(w, "%s %d\n", step, weights[step])
	}
	return w.Flush()
}

// runOne runs one node's script and links its output into lib. Its trace
// has the script's end, the link's end and the script's CPU time.
func runOne(n *runNode, lib string) ([]byte, nodeTrace, error) {
	var tr nodeTrace
	script := filepath.Join(os.TempDir(), n.Name+".sh")
	if err := os.WriteFile(script, []byte(n.Script), 0o644); err != nil {
		return nil, tr, err
	}
	cmd := exec.Command("sh", script)
	tr.Start = time.Now().UnixNano()
	out, err := cmd.CombinedOutput()
	tr.End = time.Now().UnixNano()
	if ps := cmd.ProcessState; ps != nil {
		tr.UserUS = ps.UserTime().Microseconds()
		tr.SysUS = ps.SystemTime().Microseconds()
		if ru, ok := ps.SysUsage().(*syscall.Rusage); ok {
			tr.MaxRSSKB = int64(ru.Maxrss)
		}
	}
	if err != nil {
		return out, tr, err
	}
	src := filepath.Join("/work", n.Name, n.Output)
	dst := filepath.Join(lib, n.Name+".a")
	if err := os.Link(src, dst); err != nil {
		data, err := os.ReadFile(src)
		if err != nil {
			return out, tr, err
		}
		if err := os.WriteFile(dst, data, 0o644); err != nil {
			return out, tr, err
		}
	}
	tr.Linked = time.Now().UnixNano()
	return out, tr, nil
}

// probedFlags are the C compiler flags the go command adds only when a probe
// of the compiler says they work.
var probedFlags = map[string]bool{
	"-fno-caret-diagnostics":   true,
	"-Qunused-arguments":       true,
	"-Wl,--no-gc-sections":     true,
	"-fdebug-prefix-map=a=b":   true,
	"-ffile-prefix-map=a=b":    true,
	"-gno-record-gcc-switches": true,
	"-frandom-seed=1":          true,
}

// cc runs the C compiler probes a plan prints, which go build -n answers
// "unsupported" without running, and prints "probe <compiler> <flag>
// yes|no" for each. For each directory a cgo step runs in it prints
// "modroot <dir> <root>", the directory of the go.mod above it, as go finds
// a module's root (-modfile names another file but not another root):
// -trimpath maps a package's C sources by it. And it prints "goos <GOOS>",
// the target, whose C source mappings differ on Windows.
func cc(planPath string) error {
	data, err := os.ReadFile(planPath)
	if err != nil {
		return err
	}
	start, err := os.Getwd()
	if err != nil {
		return err
	}
	type probe struct {
		compiler, flag, cmd string
		ok                  bool
	}
	var probes []*probe
	seen := map[string]bool{}
	add := func(compiler, flag, cmd string) {
		if !seen[compiler+" "+flag] {
			seen[compiler+" "+flag] = true
			probes = append(probes, &probe{compiler: compiler, flag: flag, cmd: cmd})
		}
	}
	var dirs []string
	seenDir := map[string]bool{}
	dir := start
	for _, line := range strings.Split(string(data), "\n") {
		if d, ok := strings.CutPrefix(line, "cd "); ok {
			dir = d
			if d == "." {
				dir = start
			}
			continue
		}
		if strings.Contains(line, "/cgo ") && strings.Contains(line, " -objdir ") && !seenDir[dir] {
			seenDir[dir] = true
			dirs = append(dirs, dir)
		}
		cmd, ok := strings.CutSuffix(line, " || true")
		if !ok || !strings.Contains(cmd, " -o /dev/null") {
			continue
		}
		f := strings.Fields(cmd)
		compiler := strings.Trim(f[0], "'\"")
		for _, w := range f[1:] {
			if probedFlags[w] {
				add(compiler, w, cmd)
				// Probed only once this one works, so -n never prints it.
				if w == "-fdebug-prefix-map=a=b" {
					add(compiler, "-ffile-prefix-map=a=b", strings.Replace(cmd, w, "-ffile-prefix-map=a=b", 1))
				}
				break
			}
		}
	}
	var wg sync.WaitGroup
	for _, p := range probes {
		wg.Add(1)
		go func() {
			defer wg.Done()
			tmp, err := os.MkdirTemp("", "probe")
			if err != nil {
				return
			}
			defer os.RemoveAll(tmp)
			c := exec.Command("sh", "-c", p.cmd)
			c.Dir = tmp
			c.Env = append(os.Environ(), "LC_ALL=C")
			out, _ := c.CombinedOutput()
			// The go command's own test of the output.
			p.ok = true
			for _, no := range []string{"unrecognized", "unknown", "unrecognised", "is not supported", "not recognized", "unsupported"} {
				if bytes.Contains(out, []byte(no)) {
					p.ok = false
				}
			}
		}()
	}
	wg.Wait()
	w := bufio.NewWriter(os.Stdout)
	for _, p := range probes {
		answer := "no"
		if p.ok {
			answer = "yes"
		}
		fmt.Fprintf(w, "probe\t%s\t%s\t%s\n", p.compiler, p.flag, answer)
	}
	for _, d := range dirs {
		for r := d; filepath.IsAbs(r); r = filepath.Dir(r) {
			if fi, err := os.Stat(filepath.Join(r, "go.mod")); err == nil && !fi.IsDir() {
				fmt.Fprintf(w, "modroot\t%s\t%s\n", d, r)
				break
			}
			if r == "/" {
				break
			}
		}
	}
	if len(dirs) > 0 {
		out, err := exec.Command("go", "env", "GOOS").Output()
		if err != nil {
			return fmt.Errorf("go env GOOS: %w", err)
		}
		fmt.Fprintf(w, "goos\t%s\n", strings.TrimSpace(string(out)))
	}
	return w.Flush()
}

// linkID prints the Go build ID for a link: a hash of the link's command
// (salt), its import config and every archive the config names, so that,
// like the go command's, it changes with whatever the binary is made from.
// The linker makes the binary's GNU build ID from it.
func linkID(cfg, salt string) error {
	data, err := os.ReadFile(cfg)
	if err != nil {
		return err
	}
	var files []string
	for _, line := range strings.Split(string(data), "\n") {
		if rest, ok := strings.CutPrefix(line, "packagefile "); ok {
			if _, f, ok := strings.Cut(rest, "="); ok {
				files = append(files, f)
			}
		}
	}
	sums := make([][]byte, len(files))
	errs := make([]error, len(files))
	sem := make(chan struct{}, runtime.NumCPU())
	var wg sync.WaitGroup
	for i, f := range files {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			in, err := os.Open(f)
			if err != nil {
				errs[i] = err
				return
			}
			defer in.Close()
			h := sha256.New()
			_, errs[i] = io.Copy(h, in)
			sums[i] = h.Sum(nil)
		}()
	}
	wg.Wait()
	h := sha256.New()
	fmt.Fprintf(h, "%s\n%s", salt, data)
	for i := range files {
		if errs[i] != nil {
			return errs[i]
		}
		h.Write(sums[i])
	}
	// As the go command writes a hash in a build ID.
	id := base64.RawURLEncoding.EncodeToString(h.Sum(nil)[:15])
	fmt.Println(id + "/" + id + "/" + id + "/" + id)
	return nil
}

func main() {
	args := os.Args[1:]
	var err error
	switch {
	case len(args) == 3 && args[0] == "unpack":
		err = unpack(args[1], args[2])
	case len(args) == 2 && args[0] == "run":
		err = run(args[1])
	case len(args) == 2 && args[0] == "weigh":
		err = weigh(args[1])
	case len(args) == 2 && args[0] == "cc":
		err = cc(args[1])
	case len(args) == 3 && args[0] == "linkid":
		err = linkID(args[1], args[2])
	default:
		err = fmt.Errorf("usage: gohdr unpack <pack> <dir>, gohdr run <spec>, gohdr weigh <plan>, gohdr cc <plan>, or gohdr linkid <importcfg> <salt>")
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "gohdr:", err)
		os.Exit(1)
	}
}
`
