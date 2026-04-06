package main

import (
	"bytes"
	"errors"
	"flag"
	"fmt"
	"go/ast"
	gformat "go/format"
	"go/parser"
	"go/token"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/vearutop/gocan/internal/diff"
	"github.com/vearutop/gocan/internal/format"
)

func main() {
	var (
		writeFlag    bool
		listFlag     bool
		diffFlag     bool
		checkFlag    bool
		diffFile     string
		parentCommit string
		configPath   string
		ghAnnotate   bool
		ghHead       string
		ghMessage    string
		ghBase       string
	)
	flag.BoolVar(&writeFlag, "w", false, "write result to (source) file instead of stdout")
	flag.BoolVar(&listFlag, "l", false, "list files whose formatting differs from gocan's")
	flag.BoolVar(&diffFlag, "d", false, "display diffs instead of rewriting files")
	flag.BoolVar(&checkFlag, "check", false, "exit non-zero if any file is not formatted")
	flag.StringVar(&diffFile, "diff", "", "git diff file for changes (optional, used with -denoise)")
	flag.StringVar(&parentCommit, "parent-commit", "", "git parent commit for diff base (optional, used with -denoise)")
	flag.StringVar(&configPath, "config", "", "path to JSON config file")
	flag.BoolVar(&ghAnnotate, "gh-annotate", false, "emit GitHub Actions annotations for layout-only changes")
	flag.StringVar(&ghHead, "gh-head", "HEAD", "git head ref/sha for -gh-annotate")
	flag.StringVar(&ghMessage, "gh-message", "Layout-only change (canonicalization)", "GitHub Actions annotation message")
	flag.StringVar(&ghBase, "gh-base", "", "git base ref/sha for -gh-annotate (defaults to GITHUB_BASE_SHA if set)")
	flag.Parse()

	paths := flag.Args()
	if len(paths) == 0 {
		paths = []string{"-"}
	}

	if ghAnnotate {
		if err := runGHAnnotate(ghBase, ghHead, ghMessage); err != nil {
			fatal(err)
		}
		return
	}

	files, err := collectFiles(paths)
	if err != nil {
		fatal(err)
	}
	if len(files) == 0 {
		return
	}

	var (
		cfg       format.Config
		baseDir   string
		cfgLoaded bool
	)
	if configPath != "" {
		var err error
		cfg, err = format.LoadConfig(configPath)
		if err != nil {
			fatal(err)
		}
		if err := format.ValidateConfig(cfg); err != nil {
			fatal(err)
		}
		baseDir = filepath.Dir(configPath)
		cfgLoaded = true
	}

	resolver := newConfigResolver()
	out := io.Writer(os.Stdout)
	changedAny := false
	for _, path := range files {
		if path == "-" {
			stdinCfg := cfg
			if !cfgLoaded {
				stdinCfg = format.DefaultConfig()
				if err := format.ValidateConfig(stdinCfg); err != nil {
					fatal(err)
				}
			}
			src, err := io.ReadAll(os.Stdin)
			if err != nil {
				fatal(err)
			}
			formatted, err := format.FormatFile("stdin", src, stdinCfg)
			if err != nil {
				fatal(err)
			}
			changed := !bytesEqual(src, formatted)
			if changed {
				changedAny = true
			}
			if diffFlag && changed {
				diff, err := unifiedDiff("stdin", src, formatted)
				if err != nil {
					fatal(err)
				}
				if _, err := out.Write(diff); err != nil {
					fatal(err)
				}
			}
			if listFlag && changed {
				if _, err := fmt.Fprintln(out, "stdin"); err != nil {
					fatal(err)
				}
			}
			if !writeFlag && !listFlag && !diffFlag {
				if _, err := out.Write(formatted); err != nil {
					fatal(err)
				}
				if len(formatted) == 0 || formatted[len(formatted)-1] != '\n' {
					if _, err := out.Write([]byte("\n")); err != nil {
						fatal(err)
					}
				}
			}
			continue
		}

		if !cfgLoaded {
			var err error
			cfg, baseDir, err = resolver.Resolve(path)
			if err != nil {
				fatal(err)
			}
		}
		if format.IsExcluded(path, baseDir, cfg) {
			continue
		}

		src, err := os.ReadFile(path)
		if err != nil {
			fatal(err)
		}
		formatted, err := format.FormatFile(path, src, cfg)
		if err != nil {
			fatal(err)
		}
		changed := !bytesEqual(src, formatted)
		if changed {
			changedAny = true
		}

		if listFlag && changed {
			if _, err := fmt.Fprintln(out, path); err != nil {
				fatal(err)
			}
		}

		if diffFlag && changed {
			diff, err := unifiedDiff(path, src, formatted)
			if err != nil {
				fatal(err)
			}
			if _, err := out.Write(diff); err != nil {
				fatal(err)
			}
		}

		if writeFlag && changed {
			if err := os.WriteFile(path, formatted, 0o644); err != nil {
				fatal(err)
			}
		}

		if !writeFlag && !listFlag && !diffFlag {
			if _, err := out.Write(formatted); err != nil {
				fatal(err)
			}
			if len(formatted) == 0 || formatted[len(formatted)-1] != '\n' {
				if _, err := out.Write([]byte("\n")); err != nil {
					fatal(err)
				}
			}
		}
	}

	if checkFlag && changedAny {
		os.Exit(1)
	}
}

func bytesEqual(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func collectFiles(paths []string) ([]string, error) {
	var files []string
	for _, p := range paths {
		if p == "-" {
			files = append(files, p)
			continue
		}
		p = format.CanonicalizePath(p)
		info, err := os.Stat(p)
		if err != nil {
			return nil, err
		}
		if info.IsDir() {
			root := p
			err := filepath.WalkDir(p, func(path string, d os.DirEntry, err error) error {
				if err != nil {
					return err
				}
				if d.IsDir() {
					if path == root {
						return nil
					}
					name := d.Name()
					if name == "vendor" || strings.HasPrefix(name, ".") {
						return filepath.SkipDir
					}
					return nil
				}
				if strings.HasSuffix(path, ".go") {
					files = append(files, path)
				}
				return nil
			})
			if err != nil {
				return nil, err
			}
			continue
		}
		if strings.HasSuffix(p, ".go") {
			files = append(files, p)
		}
	}
	return files, nil
}

func runGHAnnotate(baseRef, headRef, message string) error {
	if err := requireGitWorktree(); err != nil {
		return err
	}
	var tried []string
	resolvedBase, err := resolveBaseRef(baseRef, &tried)
	if err != nil {
		return err
	}
	baseRef = resolvedBase
	debug := debugEnabled()
	debugf(debug, "gocan -gh-annotate base=%q head=%q message=%q", baseRef, headRef, message)
	if len(tried) > 0 {
		debugf(debug, "base ref candidates tried: %s", strings.Join(tried, ", "))
	}
	baseCommit, err := mergeBase(baseRef, headRef)
	if err != nil {
		return err
	}
	debugf(debug, "merge base: %s", baseCommit)
	files, err := changedGoFiles(baseCommit, headRef)
	if err != nil {
		if len(tried) > 0 {
			return fmt.Errorf("git diff failed for base %q and head %q: %w (tried: %s)", baseRef, headRef, err, strings.Join(tried, ", "))
		}
		return err
	}
	if len(files) == 0 {
		debugf(debug, "no changed .go files")
		return nil
	}

	resolver := newConfigResolver()
	var (
		filesScanned     int
		filesWithChanges int
		filesAnnotated   int
		annotatedDecls   int
	)
	for _, path := range files {
		filesScanned++
		headSrc, err := os.ReadFile(path)
		if errors.Is(err, os.ErrNotExist) {
			debugf(debug, "skip %s: missing in head", path)
			continue
		}
		if err != nil {
			return err
		}

		cfg, baseDir, err := resolver.Resolve(path)
		if err != nil {
			return err
		}
		if format.IsExcluded(path, baseDir, cfg) {
			debugf(debug, "skip %s: excluded", path)
			continue
		}

		baseSrc, err := gitShow(baseCommit, path)
		if err != nil {
			// New file or missing in base: skip annotation.
			debugf(debug, "skip %s: missing in base %q", path, baseCommit)
			continue
		}

		rawRanges, _, err := gitDiffRanges(baseCommit, headRef, path)
		if err != nil {
			return err
		}
		if len(rawRanges) == 0 {
			debugf(debug, "skip %s: no raw hunks", path)
			continue
		}
		filesWithChanges++

		baseNorm, err := format.FormatFile(path, baseSrc, cfg)
		if err != nil {
			return err
		}
		headNorm, err := format.FormatFile(path, headSrc, cfg)
		if err != nil {
			return err
		}

		declsBase, err := declSpans(baseNorm)
		if err != nil {
			return err
		}
		declsHead, err := declSpans(headNorm)
		if err != nil {
			return err
		}
		unchanged := matchUnchangedDecls(declsBase, declsHead)
		annotations := declsOverlapping(unchanged, rawRanges)
		if len(annotations) == 0 {
			debugf(debug, "skip %s: no layout-only decls", path)
			continue
		}
		filesAnnotated++
		annotations = mergeRangesByContent(annotations, headSrc, 2)
		for _, r := range annotations {
			annotatedDecls++
			emitNotice(path, r, message)
		}
	}
	debugf(debug, "summary: files_scanned=%d files_with_changes=%d files_annotated=%d annotated_decls=%d", filesScanned, filesWithChanges, filesAnnotated, annotatedDecls)
	return nil
}

func emitNotice(path string, r hunkRange, msg string) {
	if r.Start <= 0 {
		return
	}
	if r.End < r.Start {
		r.End = r.Start
	}
	if r.End == r.Start {
		fmt.Printf("::notice file=%s,line=%d::%s\n", path, r.Start, msg)
		return
	}
	fmt.Printf("::notice file=%s,line=%d,endLine=%d::%s\n", path, r.Start, r.End, msg)
}

func diffRanges(name string, oldSrc, newSrc []byte) []hunkRange {
	d := diff.Diff(name, oldSrc, name, newSrc)
	if len(d) == 0 {
		return nil
	}
	return parseUnifiedRanges(d)
}

func parseUnifiedRanges(unified []byte) []hunkRange {
	var ranges []hunkRange
	for _, line := range bytesSplit(unified, '\n') {
		if !strings.HasPrefix(line, "@@") {
			continue
		}
		parts := strings.Fields(line)
		if len(parts) < 3 {
			continue
		}
		start, count, ok := parseRange(parts[2])
		if !ok {
			continue
		}
		if count == 0 {
			continue
		}
		ranges = append(ranges, hunkRange{Start: start, End: start + count - 1})
	}
	return ranges
}

func parseRange(part string) (start, count int, ok bool) {
	part = strings.TrimSpace(part)
	if part == "" {
		return 0, 0, false
	}
	if part[0] == '+' || part[0] == '-' {
		part = part[1:]
	}
	if part == "" {
		return 0, 0, false
	}
	if i := strings.IndexByte(part, ','); i >= 0 {
		s, err := strconv.Atoi(part[:i])
		if err != nil {
			return 0, 0, false
		}
		c, err := strconv.Atoi(part[i+1:])
		if err != nil {
			return 0, 0, false
		}
		return s, c, true
	}
	s, err := strconv.Atoi(part)
	if err != nil {
		return 0, 0, false
	}
	return s, 1, true
}

type hunkRange struct {
	Start int
	End   int
}

func mergeRangesByContent(ranges []hunkRange, src []byte, gap int) []hunkRange {
	if len(ranges) <= 1 {
		return ranges
	}
	sort.Slice(ranges, func(i, j int) bool {
		return ranges[i].Start < ranges[j].Start
	})
	merged := []hunkRange{ranges[0]}
	lines := bytesSplit(src, '\n')
	for _, r := range ranges[1:] {
		last := &merged[len(merged)-1]
		if r.Start <= last.End+gap && gapMergeable(lines, last.End+1, r.Start-1) {
			if r.End > last.End {
				last.End = r.End
			}
			continue
		}
		merged = append(merged, r)
	}
	return merged
}

func changedGoFiles(baseRef, headRef string) ([]string, error) {
	args := []string{
		"diff",
		"--name-only",
		"--diff-filter=ACMRT",
		fmt.Sprintf("%s..%s", baseRef, headRef),
		"--",
		"*.go",
	}
	out, err := execGitDiff(args...)
	if err != nil {
		return nil, fmt.Errorf("git diff failed for base %q and head %q: %w", baseRef, headRef, err)
	}
	var files []string
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		files = append(files, format.CanonicalizePath(line))
	}
	return files, nil
}

func gitShow(ref, path string) ([]byte, error) {
	arg := fmt.Sprintf("%s:%s", ref, path)
	return execCommand("git", "show", arg)
}

func execCommand(name string, args ...string) ([]byte, error) {
	cmd := exec.Command(name, args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		msg := strings.TrimSpace(string(out))
		if msg == "" {
			return nil, err
		}
		return nil, fmt.Errorf("%w: %s", err, msg)
	}
	return out, nil
}

func execGitDiff(args ...string) ([]byte, error) {
	cmd := exec.Command("git", args...)
	out, err := cmd.CombinedOutput()
	if err == nil {
		return out, nil
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) && exitErr.ExitCode() == 1 {
		// Exit code 1 means "differences found".
		return out, nil
	}
	msg := strings.TrimSpace(string(out))
	if msg == "" {
		return nil, err
	}
	return nil, fmt.Errorf("%w: %s", err, msg)
}

func requireGitWorktree() error {
	out, err := exec.Command("git", "rev-parse", "--is-inside-work-tree").CombinedOutput()
	if err == nil && strings.TrimSpace(string(out)) == "true" {
		return nil
	}
	msg := strings.TrimSpace(string(out))
	if msg == "" {
		msg = "not a git worktree"
	}
	return fmt.Errorf("gocan -gh-annotate requires a git worktree: %s", msg)
}

func resolveBaseRef(baseRef string, tried *[]string) (string, error) {
	baseRef = strings.TrimSpace(baseRef)
	if baseRef != "" {
		return baseRef, nil
	}
	if env := strings.TrimSpace(os.Getenv("GITHUB_BASE_SHA")); env != "" {
		*tried = append(*tried, "GITHUB_BASE_SHA="+env)
		if refExists(env) {
			return env, nil
		}
	}
	candidates := []string{"origin/main", "origin/master", "main", "master"}
	for _, c := range candidates {
		*tried = append(*tried, c)
		if refExists(c) {
			return c, nil
		}
	}
	return "", fmt.Errorf("unable to resolve git base ref; provide -gh-base or set GITHUB_BASE_SHA")
}

func refExists(ref string) bool {
	_, err := execCommand("git", "rev-parse", "--verify", ref)
	return err == nil
}

func mergeBase(baseRef, headRef string) (string, error) {
	out, err := execCommand("git", "merge-base", baseRef, headRef)
	if err != nil {
		return "", fmt.Errorf("failed to resolve merge base for %q and %q: %w", baseRef, headRef, err)
	}
	mb := strings.TrimSpace(string(out))
	if mb == "" {
		return "", fmt.Errorf("merge base for %q and %q is empty", baseRef, headRef)
	}
	return mb, nil
}

func debugEnabled() bool {
	v := strings.TrimSpace(os.Getenv("GOCAN_DEBUG"))
	if v == "" {
		return false
	}
	switch strings.ToLower(v) {
	case "0", "false", "no", "off":
		return false
	default:
		return true
	}
}

func debugf(enabled bool, format string, args ...any) {
	if !enabled {
		return
	}
	fmt.Fprintf(os.Stderr, "gocan debug: "+format+"\n", args...)
}

func bytesSplit(b []byte, sep byte) []string {
	if len(b) == 0 {
		return nil
	}
	out := []string{}
	start := 0
	for i := 0; i < len(b); i++ {
		if b[i] == sep {
			out = append(out, string(b[start:i]))
			start = i + 1
		}
	}
	if start <= len(b) {
		out = append(out, string(b[start:]))
	}
	return out
}

func previewLines(b []byte, maxLines int) string {
	lines := bytesSplit(b, '\n')
	if len(lines) > maxLines {
		lines = lines[:maxLines]
	}
	return strings.Join(lines, "\n")
}

func gapMergeable(lines []string, startLine, endLine int) bool {
	if startLine > endLine {
		return true
	}
	if startLine < 1 {
		startLine = 1
	}
	if endLine > len(lines) {
		endLine = len(lines)
	}
	inBlock := false
	for i := startLine; i <= endLine; i++ {
		line := strings.TrimSpace(lines[i-1])
		if line == "" {
			continue
		}
		if inBlock {
			if strings.Contains(line, "*/") {
				inBlock = false
			}
			continue
		}
		if strings.HasPrefix(line, "//") {
			continue
		}
		if strings.HasPrefix(line, "/*") {
			if !strings.Contains(line, "*/") {
				inBlock = true
			}
			continue
		}
		if strings.HasPrefix(line, "*") || strings.HasPrefix(line, "*/") {
			continue
		}
		return false
	}
	return true
}

func gitDiffRanges(baseRef, headRef, path string) ([]hunkRange, []byte, error) {
	out, err := execGitDiff("diff", "--unified=0", "--no-color", baseRef, headRef, "--", path)
	if err != nil {
		return nil, nil, err
	}
	return parseUnifiedRanges(out), out, nil
}

type declSpan struct {
	Fingerprint string
	StartLine   int
	EndLine     int
}

func declSpans(src []byte) ([]declSpan, error) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "file.go", src, 0)
	if err != nil {
		return nil, err
	}
	spans := make([]declSpan, 0, len(file.Decls))
	for _, decl := range file.Decls {
		gen, ok := decl.(*ast.GenDecl)
		if ok && gen.Tok == token.IMPORT {
			continue
		}
		fp, err := declFingerprint(fset, decl)
		if err != nil {
			return nil, err
		}
		start := fset.Position(decl.Pos()).Line
		end := fset.Position(decl.End()).Line
		if end < start {
			end = start
		}
		spans = append(spans, declSpan{
			Fingerprint: fp,
			StartLine:   start,
			EndLine:     end,
		})
	}
	return spans, nil
}

func declFingerprint(fset *token.FileSet, decl ast.Decl) (string, error) {
	var buf bytes.Buffer
	if err := gformat.Node(&buf, fset, decl); err != nil {
		return "", err
	}
	kind, name := declKindName(decl)
	return kind + ":" + name + ":" + buf.String(), nil
}

func declKindName(decl ast.Decl) (string, string) {
	switch d := decl.(type) {
	case *ast.FuncDecl:
		return "func", d.Name.Name
	case *ast.GenDecl:
		switch d.Tok {
		case token.CONST:
			return "const", genDeclName(d)
		case token.VAR:
			return "var", genDeclName(d)
		case token.TYPE:
			return "type", genDeclName(d)
		default:
			return d.Tok.String(), genDeclName(d)
		}
	default:
		return "decl", ""
	}
}

func genDeclName(d *ast.GenDecl) string {
	if len(d.Specs) == 0 {
		return ""
	}
	switch s := d.Specs[0].(type) {
	case *ast.TypeSpec:
		return s.Name.Name
	case *ast.ValueSpec:
		if len(s.Names) > 0 {
			return s.Names[0].Name
		}
	}
	return ""
}

func matchUnchangedDecls(base, head []declSpan) []declSpan {
	baseMap := map[string][]declSpan{}
	for _, d := range base {
		baseMap[d.Fingerprint] = append(baseMap[d.Fingerprint], d)
	}
	headMap := map[string][]declSpan{}
	for _, d := range head {
		headMap[d.Fingerprint] = append(headMap[d.Fingerprint], d)
	}
	var unchanged []declSpan
	for fp, headList := range headMap {
		baseList := baseMap[fp]
		n := len(headList)
		if len(baseList) < n {
			n = len(baseList)
		}
		if n == 0 {
			continue
		}
		unchanged = append(unchanged, headList[:n]...)
	}
	return unchanged
}

func declsOverlapping(decls []declSpan, ranges []hunkRange) []hunkRange {
	var out []hunkRange
	for _, d := range decls {
		for _, r := range ranges {
			if d.StartLine <= r.End && r.Start <= d.EndLine {
				out = append(out, hunkRange{Start: d.StartLine, End: d.EndLine})
				break
			}
		}
	}
	return out
}

func fatal(err error) {
	var pathErr *os.PathError
	if errors.As(err, &pathErr) {
		fmt.Fprintf(os.Stderr, "gocan: %v\n", pathErr)
		os.Exit(1)
	}
	fmt.Fprintf(os.Stderr, "gocan: %v\n", err)
	os.Exit(1)
}

func unifiedDiff(path string, a, b []byte) ([]byte, error) {
	out := diff.Diff(path, a, path, b)
	return out, nil
}
