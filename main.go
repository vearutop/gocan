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
	pkgCache := newPackageCache(baseCommit, resolver)
	var (
		filesScanned     int
		filesWithChanges int
		filesAnnotated   int
		annotatedDecls   int
	)
	for _, path := range files {
		filesScanned++
		headSrc := pkgCache.headSource(path)
		if headSrc == nil {
			debugf(debug, "skip %s: missing in head or excluded", path)
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

		unchanged := pkgCache.unchangedDecls(path)
		if len(unchanged) == 0 {
			debugf(debug, "skip %s: no layout-only decls", path)
			continue
		}
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

func declSpans(src []byte) ([]declSpan, string, error) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "file.go", src, 0)
	if err != nil {
		return nil, "", err
	}
	spans := make([]declSpan, 0, len(file.Decls))
	for _, decl := range file.Decls {
		gen, ok := decl.(*ast.GenDecl)
		if ok && gen.Tok == token.IMPORT {
			continue
		}
		fp, err := declFingerprint(fset, decl)
		if err != nil {
			return nil, "", err
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
	pkgName := ""
	if file.Name != nil {
		pkgName = file.Name.Name
	}
	return spans, pkgName, nil
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

type packageContext struct {
	baseCounts        map[string]int
	headDeclsByFile   map[string][]declSpan
	headSrcByFile     map[string][]byte
	unchangedByFile   map[string][]declSpan
	unchangedComputed bool
}

type packageCache struct {
	baseCommit string
	resolver   *configResolver
	contexts   map[string]*packageContext
	fileToKey  map[string]string
}

func newPackageCache(baseCommit string, resolver *configResolver) *packageCache {
	return &packageCache{
		baseCommit: baseCommit,
		resolver:   resolver,
		contexts:   make(map[string]*packageContext),
		fileToKey:  make(map[string]string),
	}
}

func (c *packageCache) headSource(path string) []byte {
	if key, ok := c.fileToKey[path]; ok {
		if ctx, ok := c.contexts[key]; ok {
			return ctx.headSrcByFile[path]
		}
	}
	_ = c.ensureDir(filepath.Dir(path))
	if key, ok := c.fileToKey[path]; ok {
		if ctx, ok := c.contexts[key]; ok {
			return ctx.headSrcByFile[path]
		}
	}
	return nil
}

func (c *packageCache) unchangedDecls(path string) []declSpan {
	if key, ok := c.fileToKey[path]; ok {
		if ctx, ok := c.contexts[key]; ok {
			if !ctx.unchangedComputed {
				c.computeUnchanged(ctx)
			}
			return ctx.unchangedByFile[path]
		}
	}
	_ = c.ensureDir(filepath.Dir(path))
	if key, ok := c.fileToKey[path]; ok {
		if ctx, ok := c.contexts[key]; ok {
			if !ctx.unchangedComputed {
				c.computeUnchanged(ctx)
			}
			return ctx.unchangedByFile[path]
		}
	}
	return nil
}

func (c *packageCache) ensureDir(dir string) error {
	headFiles, err := listGoFilesInDir(dir)
	if err != nil {
		return err
	}
	baseFiles, err := listGoFilesInDirAtCommit(c.baseCommit, dir)
	if err != nil {
		return err
	}

	for _, path := range headFiles {
		if _, ok := c.fileToKey[path]; ok {
			continue
		}
		cfg, baseDir, err := c.resolver.Resolve(path)
		if err != nil {
			return err
		}
		if format.IsExcluded(path, baseDir, cfg) {
			continue
		}
		src, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		norm, err := format.FormatFile(path, src, cfg)
		if err != nil {
			return err
		}
		decls, pkg, err := declSpans(norm)
		if err != nil {
			return err
		}
		key := packageKey(dir, pkg)
		ctx := c.contexts[key]
		if ctx == nil {
			ctx = &packageContext{
				baseCounts:      make(map[string]int),
				headDeclsByFile: make(map[string][]declSpan),
				headSrcByFile:   make(map[string][]byte),
				unchangedByFile: make(map[string][]declSpan),
			}
			c.contexts[key] = ctx
		}
		c.fileToKey[path] = key
		ctx.headDeclsByFile[path] = decls
		ctx.headSrcByFile[path] = src
	}

	for _, path := range baseFiles {
		cfg, baseDir, err := c.resolver.Resolve(path)
		if err != nil {
			return err
		}
		if format.IsExcluded(path, baseDir, cfg) {
			continue
		}
		src, err := gitShow(c.baseCommit, path)
		if err != nil {
			continue
		}
		norm, err := format.FormatFile(path, src, cfg)
		if err != nil {
			return err
		}
		decls, pkg, err := declSpans(norm)
		if err != nil {
			return err
		}
		key := packageKey(dir, pkg)
		ctx := c.contexts[key]
		if ctx == nil {
			ctx = &packageContext{
				baseCounts:      make(map[string]int),
				headDeclsByFile: make(map[string][]declSpan),
				headSrcByFile:   make(map[string][]byte),
				unchangedByFile: make(map[string][]declSpan),
			}
			c.contexts[key] = ctx
		}
		for _, d := range decls {
			ctx.baseCounts[d.Fingerprint]++
		}
	}
	return nil
}

func (c *packageCache) computeUnchanged(ctx *packageContext) {
	counts := make(map[string]int, len(ctx.baseCounts))
	for k, v := range ctx.baseCounts {
		counts[k] = v
	}
	files := make([]string, 0, len(ctx.headDeclsByFile))
	for f := range ctx.headDeclsByFile {
		files = append(files, f)
	}
	sort.Strings(files)
	for _, f := range files {
		for _, d := range ctx.headDeclsByFile[f] {
			if counts[d.Fingerprint] > 0 {
				ctx.unchangedByFile[f] = append(ctx.unchangedByFile[f], d)
				counts[d.Fingerprint]--
			}
		}
	}
	ctx.unchangedComputed = true
}

func packageKey(dir, pkg string) string {
	return dir + "||" + pkg
}

func listGoFilesInDir(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var files []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if strings.HasSuffix(name, ".go") {
			files = append(files, filepath.Join(dir, name))
		}
	}
	return files, nil
}

func listGoFilesInDirAtCommit(commit, dir string) ([]string, error) {
	out, err := execCommand("git", "ls-tree", "-r", "--name-only", commit, "--", dir)
	if err != nil {
		return nil, err
	}
	var files []string
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if !strings.HasSuffix(line, ".go") {
			continue
		}
		if filepath.Dir(line) != dir {
			continue
		}
		files = append(files, line)
	}
	return files, nil
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
