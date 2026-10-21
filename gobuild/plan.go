package main

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"path"
	"regexp"
	"sort"
	"strings"
)

// roots are where a plan's files live: the workspace source at its mount
// path, module downloads at GOMODCACHE, and the build's working directory.
type roots struct {
	Src string `json:"src"`
	Mod string `json:"mod"`
	Cwd string `json:"cwd"`
}

// block is one package's commands from a go build -n plan.
type block struct {
	id         string // bNNN in the plan
	importPath string
	name       string // stable directory name under /work
	lines      []string
	deps       []string // other block ids
	srcFiles   []string // relative to roots.Src; may hold a "**/*.h" pattern
	modFiles   []string // relative to roots.Mod; may hold "<mod>@<v>/**/*.h"
	// isLink marks the main package's link, split from its compile (b001)
	// so an edit that leaves the main package's archive unchanged relinks
	// without recompiling it.
	isLink bool
	group  string // std, dep or ws, for the timeline
}

var (
	reMkdir       = regexp.MustCompile(`^mkdir -p \$WORK/(b\d+)/`)
	reWorkRef     = regexp.MustCompile(`\$WORK/(b\d+)`)
	reRelFile     = regexp.MustCompile(`(^|\s)\./([^\s"';]+)`)
	reBuildID     = regexp.MustCompile(`\s-buildid[ =]\S+`)
	reBuildIDStep = regexp.MustCompile(`^(go tool buildid|\S+/buildid) -w `)
	reConc        = regexp.MustCompile(`\s-c=\d+`)
	reDepArchive  = regexp.MustCompile(`\$WORK/(b\d+)/_pkg_\.a`)
	// cat >TARGET << 'EOF' [# internal]
	reHeredoc = regexp.MustCompile(`^cat >(\S+) << 'EOF'( # internal)?$`)
	// mkdir -p DIR [&& REST]
	reMkdirLine = regexp.MustCompile(`^mkdir -p (\S+)(?: && (.*))?$`)
	// The link's output: exe/a.out, or exe/a.out.exe for Windows.
	reLinkOut = regexp.MustCompile(`\s-o \$WORK/b001/(exe/\S+)`)
)

// planOut is where the plan's go build writes the binary. The plan's own
// mkdir and mv for it are dropped: the binary is read from the link's exec.
const planOut = "/gobuild/out"

// linkID is the block id of the main package's link.
const linkID = "link"

// pathRegexp matches an absolute path under the source or module root that
// starts a token, so a path such as /usr/local/go/src/... does not match.
func pathRegexp(r roots) *regexp.Regexp {
	return regexp.MustCompile(`(?:^|[\s"'=])((?:` + regexp.QuoteMeta(r.Src) + `|` + regexp.QuoteMeta(r.Mod) + `)(?:/[^\s"';=]*)?)`)
}

func stableName(importPath string) string {
	sum := sha256.Sum256([]byte(importPath))
	return "p" + hex.EncodeToString(sum[:6])
}

// parsePlan splits a go build -n plan into per-package blocks and records
// each block's input files and dependencies.
func parsePlan(plan string, r roots) (map[string]*block, error) {
	rePath := pathRegexp(r)
	blocks := map[string]*block{}
	var cur *block
	cwd := r.Cwd
	inEmbedcfg := false
	// A package's header ("#", "# importpath", "#") comes before the mkdir
	// of its work directory in some Go releases (1.24) and after it in
	// others, so a header waits for a block without one.
	pending := ""
	lines := strings.Split(plan, "\n")
	for i := 0; i < len(lines); i++ {
		line := lines[i]
		if line == "#" && i+2 < len(lines) && strings.HasPrefix(lines[i+1], "# ") && lines[i+2] == "#" {
			if h := strings.TrimPrefix(lines[i+1], "# "); cur != nil && cur.importPath == "" {
				cur.importPath = h
			} else {
				pending = h
			}
			// Not kept: it names the package, which script writes itself, and
			// kept in place it would land in the previous block's script.
			i += 2
			continue
		}
		if m := reMkdir.FindStringSubmatch(line); m != nil {
			b, ok := blocks[m[1]]
			if !ok {
				b = &block{id: m[1], importPath: pending}
				blocks[m[1]] = b
				pending = ""
			}
			cur = b
			// The go command prints "cd" only when the directory changes, so
			// carry the current directory into each block explicitly.
			cur.lines = append(cur.lines, "mkdir -p "+cwd+" && cd "+cwd)
		}
		if cur == nil {
			continue
		}
		switch {
		// The go command's build ID steps, printed as "go tool buildid" or
		// by the tool's path: nothing here carries an ID to fill in.
		case reBuildIDStep.MatchString(line),
			strings.HasPrefix(line, "mkdir -p "+planOut),
			strings.HasPrefix(line, "mv $WORK/"),
			strings.HasPrefix(line, "rm -rf $WORK/"):
			continue
		}
		if d, ok := strings.CutPrefix(line, "cd "); ok {
			if d == "." {
				d = r.Cwd
			}
			cwd = d
		}
		// An embedcfg heredoc lists the files a package embeds, whatever
		// their extension, as JSON that may span lines.
		if strings.Contains(line, "/embedcfg << 'EOF'") {
			inEmbedcfg = true
		}
		if inEmbedcfg {
			for _, m := range rePath.FindAllStringSubmatch(line, -1) {
				addFile(cur, r, m[1])
			}
			if line == "EOF" || strings.HasSuffix(line, "}EOF") {
				inEmbedcfg = false
			}
		}
		// The go command writes embedcfg JSON without a trailing newline.
		if strings.HasSuffix(line, "}EOF") {
			cur.lines = append(cur.lines, strings.TrimSuffix(line, "EOF"), "EOF")
			continue
		}
		cur.lines = append(cur.lines, line)
		for _, m := range rePath.FindAllStringSubmatch(line, -1) {
			addSource(cur, r, m[1])
		}
		// Relative source paths appear on tool command lines and, for long
		// file lists, one per line in a heredoc arguments file.
		for _, m := range reRelFile.FindAllStringSubmatch(line, -1) {
			addSource(cur, r, path.Join(cwd, m[2]))
		}
	}
	for id, b := range blocks {
		if b.importPath != "" {
			continue
		}
		// Steps other than a package's compile and link come without a
		// package header: cgo and a PGO profile's preprocessing aren't done
		// yet, and anything else is a plan this build doesn't understand.
		for _, l := range b.lines {
			if strings.Contains(l, "/preprofile ") || strings.Contains(l, "/cgo ") {
				return nil, fmt.Errorf("%w: step %s: %s", errUnsupported, id, l)
			}
		}
		return nil, fmt.Errorf("plan step %s has no package header", id)
	}
	if err := splitLink(blocks); err != nil {
		return nil, err
	}
	for id, b := range blocks {
		for _, l := range b.lines {
			if strings.Contains(l, "/cgo ") {
				return nil, fmt.Errorf("%w: %s uses cgo", errUnsupported, b.importPath)
			}
		}
		// Assembly can #include headers from elsewhere in its module, which
		// the plan never names: mount every header of the module.
		for _, f := range b.modFiles {
			if path.Ext(f) == ".s" {
				if i := strings.Index(f, "@"); i >= 0 {
					if j := strings.Index(f[i:], "/"); j >= 0 {
						b.modFiles = appendUniq(b.modFiles, f[:i+j]+"/**/*.h")
					}
				}
			}
		}
		for _, f := range b.srcFiles {
			if path.Ext(f) == ".s" {
				b.srcFiles = appendUniq(b.srcFiles, "**/*.h")
			}
		}
		b.name = stableName(b.importPath)
		seen := map[string]bool{}
		for _, l := range b.lines {
			for _, m := range reWorkRef.FindAllStringSubmatch(l, -1) {
				if m[1] != id && !seen[m[1]] {
					seen[m[1]] = true
					b.deps = append(b.deps, m[1])
				}
			}
		}
		sort.Strings(b.deps)
		sort.Strings(b.srcFiles)
		sort.Strings(b.modFiles)
	}
	return blocks, nil
}

// errUnsupported marks what the per-package build doesn't do yet.
var errUnsupported = errors.New("unsupported for a per-package build")

// splitLink moves the main package's link out of its block (b001) into a
// block of its own. The link's lines start at its import config.
func splitLink(blocks map[string]*block) error {
	main, ok := blocks["b001"]
	if !ok {
		return fmt.Errorf("%w: no main package in the plan", errUnsupported)
	}
	at := -1
	for i, l := range main.lines {
		if strings.HasPrefix(l, "cat >$WORK/b001/importcfg.link ") {
			at = i
			break
		}
	}
	if at < 0 {
		return fmt.Errorf("%w: no link step for %s", errUnsupported, main.importPath)
	}
	blocks[linkID] = &block{
		id:         linkID,
		importPath: main.importPath,
		// The link runs in its own directory: main.lines[0] carries it.
		lines:  append([]string{main.lines[0]}, main.lines[at:]...),
		isLink: true,
	}
	main.lines = main.lines[:at]
	if strings.Contains(blocks[linkID].linkLine(), "-linkmode=external") {
		return fmt.Errorf("%w: external linking", errUnsupported)
	}
	return nil
}

func addSource(b *block, r roots, p string) {
	switch path.Ext(p) {
	case ".go", ".s", ".h", ".syso":
		addFile(b, r, p)
	}
}

// addFile records an input file of the block, relative to its root.
func addFile(b *block, r roots, p string) {
	if rel, ok := strings.CutPrefix(p, r.Src+"/"); ok {
		b.srcFiles = appendUniq(b.srcFiles, rel)
	} else if rel, ok := strings.CutPrefix(p, r.Mod+"/"); ok {
		b.modFiles = appendUniq(b.modFiles, rel)
	}
}

func appendUniq(xs []string, x string) []string {
	for _, y := range xs {
		if y == x {
			return xs
		}
	}
	return append(xs, x)
}

// script renders a block as one shell script for its exec. A dependency's
// archive is read from /work/lib, or, for the link, from /work/t<k> when tier
// k built it (outside). Nothing gets a Go build ID: the plan's are temporary,
// and leaving them out lets early cutoff compare archives by their bytes.
func (b *block) script(blocks map[string]*block, outside map[string]int) string {
	dirs := []string{"/work/" + b.name}
	var body strings.Builder
	var heredoc []string
	heredocTarget := ""
	needed := cdNeeded(b.lines)
	for i, l := range b.lines {
		if isCd(l) && !needed[i] {
			continue
		}
		l = reBuildID.ReplaceAllString(l, "")
		l = reConc.ReplaceAllString(l, " -c=4")
		// The go command packs archives in-process; "go tool pack" would
		// build the pack tool from source in every fresh container.
		l = strings.Replace(l, "go tool pack ", toolsDir+"/pack ", 1)
		l = reDepArchive.ReplaceAllStringFunc(l, func(s string) string {
			id := reDepArchive.FindStringSubmatch(s)[1]
			name := blocks[id].name
			if id == b.id {
				return "/work/" + name + "/" + name + ".a"
			}
			if k, ok := outside[id]; ok {
				return fmt.Sprintf("/work/t%d/%s.a", k, name)
			}
			return "/work/lib/" + name + ".a"
		})
		l = reWorkRef.ReplaceAllStringFunc(l, func(s string) string {
			return "/work/" + blocks[strings.TrimPrefix(s, "$WORK/")].name
		})
		if heredocTarget != "" {
			if l == "EOF" {
				body.WriteString("printf '%s\\n'")
				for _, h := range heredoc {
					body.WriteString(" " + shQuote(h))
				}
				body.WriteString(" > " + heredocTarget + "\n")
				heredoc, heredocTarget = nil, ""
				continue
			}
			heredoc = append(heredoc, l)
			continue
		}
		if m := reHeredoc.FindStringSubmatch(l); m != nil {
			heredocTarget = m[1]
			continue
		}
		if m := reMkdirLine.FindStringSubmatch(l); m != nil {
			dirs = append(dirs, m[1])
			if m[2] != "" {
				body.WriteString(m[2] + "\n")
			}
			continue
		}
		body.WriteString(l + "\n")
	}
	return "set -e\n# " + b.importPath + "\nmkdir -p " + strings.Join(dirs, " ") + "\n" + body.String()
}

// isCd reports whether a plan line changes directory: the go command's own
// "cd", or the "mkdir -p DIR && cd DIR" parsePlan starts each block with.
func isCd(l string) bool {
	return strings.HasPrefix(l, "cd ") || (strings.HasPrefix(l, "mkdir -p ") && strings.Contains(l, " && cd "))
}

// cdNeeded reports, by line index, which directory changes matter: those a
// later line uses through a relative path before the next change. The
// others are left out, so a package's exec doesn't carry the directory the
// build happened to be in (the project's mount path, or the previous
// package's directory) and two projects can share it.
func cdNeeded(lines []string) map[int]bool {
	needed := map[int]bool{}
	last := -1
	for i, l := range lines {
		if isCd(l) {
			last = i
		} else if last >= 0 && reRelFile.MatchString(l) {
			needed[last] = true
		}
	}
	return needed
}

// linkLine is the plan's link command, or "" for a block that doesn't link.
func (b *block) linkLine() string {
	if !b.isLink {
		return ""
	}
	for _, l := range b.lines {
		if strings.Contains(l, "/link ") {
			return l
		}
	}
	return ""
}

// output is the file a block's exec produces, under /work/<name>.
func (b *block) output() string {
	if m := reLinkOut.FindStringSubmatch(b.linkLine()); m != nil {
		return m[1]
	}
	return b.name + ".a"
}

func shQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
