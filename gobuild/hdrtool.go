package main

import "dagger/gobuild/internal/dagger"

// hdrTool builds gohdr, which makes the header pack the plan is keyed on. "gohdr pack"
// writes a source tree to one file, keeping what "go build -n" reads: every
// file name, each .go file up to the end of its imports plus its go:embed
// lines, and build inputs such as go.mod and assembly whole. An edit inside a
// function body leaves that file byte-identical, so a plan made from it is a
// cache hit. "gohdr unpack" writes the tree back out.
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
	"fmt"
	"go/parser"
	"go/token"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// Non-Go build inputs the go command reads beyond their names.
var keepExt = map[string]bool{".s": true, ".S": true, ".c": true, ".h": true, ".cc": true, ".cpp": true,
	".cxx": true, ".hh": true, ".hpp": true, ".hxx": true, ".m": true, ".f": true, ".F": true, ".for": true,
	".f90": true, ".swig": true, ".swigcxx": true, ".syso": true}
var keepName = map[string]bool{"go.mod": true, "go.sum": true, "go.work": true, "go.work.sum": true, "modules.txt": true}

// header returns a Go file up to the end of its import declarations, plus
// its go:embed lines. A file that does not parse is kept whole.
func header(name string, src []byte) []byte {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, name, src, parser.ImportsOnly|parser.ParseComments)
	if err != nil || f == nil {
		return src
	}
	end := f.Name.End()
	for _, d := range f.Decls {
		if d.End() > end {
			end = d.End()
		}
	}
	off := fset.Position(end).Offset
	if off > len(src) {
		off = len(src)
	}
	var out bytes.Buffer
	out.Write(src[:off])
	out.WriteByte('\n')
	for _, line := range strings.Split(string(src[off:]), "\n") {
		if t := strings.TrimSpace(line); strings.HasPrefix(t, "//go:embed") {
			out.WriteString(t)
			out.WriteByte('\n')
		}
	}
	return out.Bytes()
}

type entry struct {
	rel  string
	mode fs.FileMode
	data []byte
}

func pack(src, dst string) error {
	var entries []entry
	err := filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if d.Name() == ".git" {
				return filepath.SkipDir
			}
			return nil
		}
		if !d.Type().IsRegular() {
			return nil
		}
		rel, err := filepath.Rel(src, p)
		if err != nil {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		var data []byte
		switch {
		case strings.HasSuffix(rel, ".go"):
			raw, err := os.ReadFile(p)
			if err != nil {
				return err
			}
			data = header(rel, raw)
		case keepName[d.Name()] || keepExt[filepath.Ext(rel)]:
			if data, err = os.ReadFile(p); err != nil {
				return err
			}
		}
		entries = append(entries, entry{rel, info.Mode().Perm(), data})
		return nil
	})
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	w := bufio.NewWriter(out)
	fmt.Fprintln(w, "gohdr 1")
	for _, e := range entries {
		fmt.Fprintf(w, "%s\n%o\n%d\n", e.rel, e.mode, len(e.data))
		w.Write(e.data)
		w.WriteByte('\n')
	}
	if err := w.Flush(); err != nil {
		return err
	}
	return out.Close()
}

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
		if err := os.WriteFile(p, data[:size], fs.FileMode(mode)); err != nil {
			return err
		}
	}
}

func main() {
	args := os.Args[1:]
	var err error
	switch {
	case len(args) == 3 && args[0] == "pack":
		err = pack(args[1], args[2])
	case len(args) == 3 && args[0] == "unpack":
		err = unpack(args[1], args[2])
	default:
		err = fmt.Errorf("usage: gohdr pack|unpack <from> <to>")
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "gohdr:", err)
		os.Exit(1)
	}
}
`
