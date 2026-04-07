package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"go/ast"
	gformat "go/format"
	"go/parser"
	"go/token"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/vearutop/gocan/internal/diff"
	"github.com/vearutop/gocan/internal/format"
)

func main() {
	opts, paths := parseFlags()
	changedAny, err := runMain(opts, paths)
	if err != nil {
		fatal(err)
	}
	if opts.checkFlag && changedAny {
		os.Exit(1)
	}
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
		kind, name := declKindName(decl)
		display := declDisplayName(decl, kind, name)
		start := fset.Position(decl.Pos()).Line
		end := fset.Position(decl.End()).Line
		end = max(end, start)
		spans = append(spans, declSpan{
			Fingerprint: fp,
			Kind:        kind,
			Name:        name,
			DisplayName: display,
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

func declDisplayName(decl ast.Decl, kind, name string) string {
	if fn, ok := decl.(*ast.FuncDecl); ok && fn.Recv != nil && len(fn.Recv.List) > 0 {
		recv := recvTypeName(fn.Recv.List[0].Type)
		if recv != "" {
			return recv + "." + name
		}
	}
	if gen, ok := decl.(*ast.GenDecl); ok {
		return genDeclDisplayName(gen, name)
	}
	if kind == "" {
		return name
	}
	return name
}

func declFingerprint(fset *token.FileSet, decl ast.Decl) (string, error) {
	var buf bytes.Buffer
	if err := gformat.Node(&buf, fset, decl); err != nil {
		return "", err
	}
	kind, name := declKindName(decl)
	return kind + ":" + name + ":" + buf.String(), nil
}

func formatNameList(names []string, fallback string) string {
	if len(names) == 0 {
		return fallback
	}
	if len(names) == 1 {
		return names[0]
	}
	return names[0] + ", ..."
}

func genDeclDisplayName(gen *ast.GenDecl, firstName string) string {
	if gen == nil {
		return firstName
	}
	kind := strings.ToLower(gen.Tok.String())
	isBlock := len(gen.Specs) > 1
	if !isBlock {
		if vs, ok := gen.Specs[0].(*ast.ValueSpec); ok && len(vs.Names) > 1 {
			isBlock = true
		}
	}
	names := genDeclNames(gen)
	if firstName == "" && len(names) > 0 {
		firstName = names[0]
	}
	if firstName == "" {
		if isBlock {
			return kind + " block"
		}
		return kind
	}
	if isBlock {
		return kind + " " + formatNameList(names, firstName) + " (block)"
	}
	return kind + " " + firstName
}

func genDeclNames(gen *ast.GenDecl) []string {
	var names []string
	for _, spec := range gen.Specs {
		switch s := spec.(type) {
		case *ast.TypeSpec:
			names = append(names, s.Name.Name)
		case *ast.ValueSpec:
			for _, n := range s.Names {
				names = append(names, n.Name)
			}
		}
	}
	return names
}

func recvTypeName(expr ast.Expr) string {
	switch v := expr.(type) {
	case *ast.Ident:
		return v.Name
	case *ast.StarExpr:
		if id, ok := v.X.(*ast.Ident); ok {
			return id.Name
		}
	}
	return ""
}

func runMain(opts cliOptions, paths []string) (bool, error) {
	if opts.ghAnnotate || opts.ghChecks {
		setNoticeCollector(opts.ghMaxNotices, !opts.ghSkipSummary, opts.ghAnnotate, opts.ghChecks)
		defer flushNotices()
		return false, runGHAnnotate(opts.ghBase, opts.ghHead)
	}
	if len(paths) == 0 {
		paths = []string{"-"}
	}
	files, err := collectFiles(paths)
	if err != nil {
		return false, err
	}
	if len(files) == 0 {
		return false, nil
	}
	cfg, baseDir, cfgLoaded, err := loadConfig(opts.configPath)
	if err != nil {
		return false, err
	}
	resolver := newConfigResolver()
	out := io.Writer(os.Stdout)
	changedAny := false
	for _, path := range files {
		changed, err := processPath(path, opts, &cfg, &baseDir, &cfgLoaded, resolver, out)
		if err != nil {
			return false, err
		}
		if changed {
			changedAny = true
		}
	}
	return changedAny, nil
}

func annotateFile(path, baseCommit, headRef string, pkgCache *packageCache, debug bool) (annotationResult, error) {
	headSrc := pkgCache.headSource(path)
	if headSrc == nil {
		debugf(debug, "skip %s: missing in head or excluded", path)
		return annotationResult{}, nil
	}
	rawRanges, _, err := gitDiffRanges(baseCommit, headRef, path)
	if err != nil {
		return annotationResult{}, err
	}
	if len(rawRanges) == 0 {
		debugf(debug, "skip %s: no raw hunks", path)
		return annotationResult{}, nil
	}
	unchanged := pkgCache.unchangedDecls(path)
	if len(unchanged) == 0 {
		debugf(debug, "skip %s: no layout-only decls", path)
		return annotationResult{hasChanges: true}, nil
	}
	annotations := declsOverlapping(unchanged, rawRanges)
	if len(annotations) == 0 {
		debugf(debug, "skip %s: no layout-only decls", path)
		return annotationResult{hasChanges: true}, nil
	}
	_ = headSrc
	for _, d := range annotations {
		emitNoticeKind(path, hunkRange{Start: d.StartLine, End: d.EndLine}, "Layout-only change", noticeLayout, d.DisplayName)
	}
	return annotationResult{hasChanges: true, annotatedDecls: len(annotations)}, nil
}

func annotateMoves(files []string, baseCommit string, resolver *configResolver, debug bool) error {
	baseMap := map[string][]declOccurrence{}
	headMap := map[string][]declOccurrence{}
	for _, path := range files {
		cfg, baseDir, err := resolver.Resolve(path)
		if err != nil {
			return err
		}
		if format.IsExcluded(path, baseDir, cfg) {
			continue
		}
		if src, err := os.ReadFile(path); err == nil {
			occ, err := buildOccurrences(path, src, cfg)
			if err != nil {
				return err
			}
			for _, o := range occ {
				headMap[o.Fingerprint] = append(headMap[o.Fingerprint], o)
			}
		}
		baseSrc, err := gitShow(baseCommit, path)
		if err != nil {
			continue
		}
		occ, err := buildOccurrences(path, baseSrc, cfg)
		if err != nil {
			return err
		}
		for _, o := range occ {
			baseMap[o.Fingerprint] = append(baseMap[o.Fingerprint], o)
		}
	}

	for fp, headList := range headMap {
		baseList := baseMap[fp]
		if err := annotateMovesForFingerprint(baseList, headList, debug); err != nil {
			return err
		}
	}
	return nil
}

func annotateMovesForFingerprint(baseList, headList []declOccurrence, debug bool) error {
	added, removed := diffOccurrencesByPackage(baseList, headList)
	if len(added) == 0 || len(removed) == 0 {
		return nil
	}
	used := make([]bool, len(removed))
	for _, add := range added {
		candidates := candidateMoves(add, removed, used)
		if len(candidates) != 1 {
			if debug && len(candidates) > 1 {
				debugf(debug, "skip move for %s: ambiguous (%d candidates)", add.File, len(candidates))
			}
			continue
		}
		idx := candidates[0]
		used[idx] = true
		src := removed[idx]
		msg := fmt.Sprintf("Moved from %s (%s)", moveLabel(src), src.File)
		emitNoticeKind(add.File, hunkRange{Start: add.StartLine, End: add.EndLine}, msg, noticeMovedFrom, add.DisplayName)
		backMsg := fmt.Sprintf("Moved to %s (%s)", moveLabel(add), add.File)
		emitNoticeKind(src.File, hunkRange{Start: src.StartLine, End: src.EndLine}, backMsg, noticeMovedTo, src.DisplayName)
	}
	return nil
}

func appendDirFiles(root string, files *[]string) error {
	return filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
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
			*files = append(*files, path)
		}
		return nil
	})
}

func buildOccurrences(path string, src []byte, cfg format.Config) ([]declOccurrence, error) {
	norm, err := format.FormatFile(path, src, cfg)
	if err != nil {
		return nil, err
	}
	decls, pkg, err := declSpans(norm)
	if err != nil {
		return nil, err
	}
	pkgKey := packageKey(filepath.Dir(path), pkg)
	occ := make([]declOccurrence, 0, len(decls))
	for _, d := range decls {
		occ = append(occ, declOccurrence{
			declSpan: d,
			File:     path,
			Pkg:      pkg,
			PkgKey:   pkgKey,
		})
	}
	return occ, nil
}

func candidateMoves(add declOccurrence, removed []declOccurrence, used []bool) []int {
	var out []int
	for i, r := range removed {
		if used[i] {
			continue
		}
		if r.PkgKey == add.PkgKey {
			continue
		}
		out = append(out, i)
	}
	return out
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

func changedGoFilesWithRenames(baseRef, headRef string) ([]string, error) {
	args := []string{
		"diff",
		"--name-status",
		"--diff-filter=ACMRD",
		fmt.Sprintf("%s..%s", baseRef, headRef),
		"--",
		"*.go",
	}
	out, err := execGitDiff(args...)
	if err != nil {
		return nil, fmt.Errorf("git diff failed for base %q and head %q: %w", baseRef, headRef, err)
	}
	seen := map[string]struct{}{}
	var files []string
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		parts := strings.Split(line, "\t")
		if len(parts) < 2 {
			continue
		}
		status := parts[0]
		addPath := func(p string) {
			p = strings.TrimSpace(p)
			if p == "" || !strings.HasSuffix(p, ".go") {
				return
			}
			p = format.CanonicalizePath(p)
			if _, ok := seen[p]; ok {
				return
			}
			seen[p] = struct{}{}
			files = append(files, p)
		}
		switch status[0] {
		case 'R', 'C':
			if len(parts) >= 3 {
				addPath(parts[1])
				addPath(parts[2])
			}
		default:
			addPath(parts[1])
		}
	}
	return files, nil
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
			if err := appendDirFiles(p, &files); err != nil {
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

func declsOverlapping(decls []declSpan, ranges []hunkRange) []declSpan {
	var out []declSpan
	for _, d := range decls {
		for _, r := range ranges {
			if d.StartLine <= r.End && r.Start <= d.EndLine {
				out = append(out, d)

				break
			}
		}
	}
	return out
}

func diffOccurrencesByPackage(base, head []declOccurrence) (added, removed []declOccurrence) {
	baseByPkg := map[string][]declOccurrence{}
	for _, b := range base {
		baseByPkg[b.PkgKey] = append(baseByPkg[b.PkgKey], b)
	}
	headByPkg := map[string][]declOccurrence{}
	for _, h := range head {
		headByPkg[h.PkgKey] = append(headByPkg[h.PkgKey], h)
	}
	for pkgKey, baseList := range baseByPkg {
		headList := headByPkg[pkgKey]
		n := min(len(baseList), len(headList))
		baseByPkg[pkgKey] = baseList[n:]
		headByPkg[pkgKey] = headList[n:]
	}
	for _, list := range baseByPkg {
		removed = append(removed, list...)
	}
	for _, list := range headByPkg {
		added = append(added, list...)
	}
	return added, removed
}

func gitDiffRanges(baseRef, headRef, path string) ([]hunkRange, []byte, error) {
	out, err := execGitDiff("diff", "--unified=0", "--no-color", baseRef, headRef, "--", path)
	if err != nil {
		return nil, nil, err
	}
	return parseUnifiedRanges(out), out, nil
}

func loadConfig(path string) (format.Config, string, bool, error) {
	if path == "" {
		return format.Config{}, "", false, nil
	}
	cfg, err := format.LoadConfig(path)
	if err != nil {
		return format.Config{}, "", false, err
	}
	if err := format.ValidateConfig(cfg); err != nil {
		return format.Config{}, "", false, err
	}
	return cfg, filepath.Dir(path), true, nil
}

func mergeBase(baseRef, headRef string) (string, error) {
	out, err := execGitCommand("merge-base", baseRef, headRef)
	if err != nil {
		return mergeBaseFallback(err, baseRef, headRef)
	}
	mb := strings.TrimSpace(string(out))
	if mb == "" {
		return "", fmt.Errorf("merge base for %q and %q is empty", baseRef, headRef)
	}
	return mb, nil
}

func mergeBaseFallback(cmdErr error, baseRef, headRef string) (string, error) {
	baseOK := refExists(baseRef)
	headOK := refExists(headRef)
	if !baseOK {
		return missingBaseFallback(cmdErr, baseRef, headRef)
	}
	if !headOK {
		return "", fmt.Errorf("failed to resolve merge base for %q and %q: head ref not present locally: %w", baseRef, headRef, cmdErr)
	}
	// Fallback: if merge-base fails but base exists, use base directly.
	return baseRef, nil
}

func missingBaseFallback(cmdErr error, baseRef, headRef string) (string, error) {
	if err := tryFetchBase(baseRef); err != nil {
		return "", fmt.Errorf("failed to resolve merge base for %q and %q: base ref not present locally; fetch the base sha (e.g. `git fetch --no-tags --depth=1 origin %s`): %w", baseRef, headRef, baseRef, cmdErr)
	}
	if !refExists(baseRef) {
		return "", fmt.Errorf("failed to resolve merge base for %q and %q: base ref not present locally; fetch the base sha (e.g. `git fetch --no-tags --depth=1 origin %s`): %w", baseRef, headRef, baseRef, cmdErr)
	}
	if mb, ok := tryMergeBase(baseRef, headRef); ok {
		return mb, nil
	}
	return "", fmt.Errorf("failed to resolve merge base for %q and %q: base ref present but merge-base failed: %w", baseRef, headRef, cmdErr)
}

func tryMergeBase(baseRef, headRef string) (string, bool) {
	out, err := execGitCommand("merge-base", baseRef, headRef)
	if err != nil {
		return "", false
	}
	mb := strings.TrimSpace(string(out))
	if mb == "" {
		return "", false
	}
	return mb, true
}

func tryFetchBase(baseRef string) error {
	// Best-effort fetch of base ref/sha for shallow checkouts.
	if baseRef == "" || baseRef == headRefValue {
		return nil
	}
	_, err := execGitCommand("fetch", "--no-tags", "--depth=1", "origin", baseRef)
	return err
}

func ensureRefAvailable(ref, remote string) error {
	if ref == "" || ref == headRefValue {
		return nil
	}
	if refExists(ref) {
		return nil
	}
	if err := tryFetchRef(remote, ref); err != nil {
		return fmt.Errorf("ref %q not present locally and fetch failed: %w", ref, err)
	}
	if !refExists(ref) {
		return fmt.Errorf("ref %q not present locally; fetch %q %q", ref, remote, ref)
	}
	return nil
}

func tryFetchRef(remote, ref string) error {
	if remote == "" {
		remote = "origin"
	}
	_, err := execGitCommand("fetch", "--no-tags", "--depth=1", remote, ref)
	return err
}

func moveFileList(baseCommit, headRef string) ([]string, error) {
	return changedGoFilesWithRenames(baseCommit, headRef)
}

func moveLabel(src declOccurrence) string {
	if src.Pkg != "" && src.DisplayName != "" {
		return src.Pkg + "." + src.DisplayName
	}
	if src.Pkg != "" && src.Name != "" {
		return src.Pkg + "." + src.Name
	}
	if src.DisplayName != "" {
		return src.DisplayName
	}
	return src.Name
}

func newPackageCache(baseCommit string, resolver *configResolver) *packageCache {
	return &packageCache{
		baseCommit: baseCommit,
		resolver:   resolver,
		contexts:   make(map[string]*packageContext),
		fileToKey:  make(map[string]string),
	}
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
	if left, right, found := strings.Cut(part, ","); found {
		s, err := strconv.Atoi(left)
		if err != nil {
			return 0, 0, false
		}
		c, err := strconv.Atoi(right)
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

func prepareAnnotate(baseRef, headRef string) (string, string, []string, []string, bool, error) {
	var triedBase []string
	var triedHead []string
	resolvedBase, err := resolveBaseRef(baseRef, &triedBase)
	if err != nil {
		return "", "", nil, nil, false, err
	}
	baseRef = resolvedBase
	resolvedHead, err := resolveHeadRef(headRef, &triedHead)
	if err != nil {
		return "", "", nil, nil, false, err
	}
	headRef = resolvedHead
	debug := debugEnabled()
	debugf(debug, "gocan -gh-annotate base=%q head=%q", baseRef, headRef)
	if len(triedBase) > 0 {
		debugf(debug, "base ref candidates tried: %s", strings.Join(triedBase, ", "))
	}
	if len(triedHead) > 0 {
		debugf(debug, "head ref candidates tried: %s", strings.Join(triedHead, ", "))
	}
	if err := ensureRefAvailable(baseRef, "origin"); err != nil {
		return "", "", nil, nil, debug, err
	}
	if err := ensureRefAvailable(headRef, "origin"); err != nil {
		return "", "", nil, nil, debug, err
	}
	baseCommit, err := mergeBase(baseRef, headRef)
	if err != nil {
		return "", "", nil, nil, debug, err
	}
	debugf(debug, "merge base: %s", baseCommit)
	files, err := changedGoFiles(baseCommit, headRef)
	if err != nil {
		if len(triedBase) > 0 {
			return "", "", nil, nil, debug, fmt.Errorf("git diff failed for base %q and head %q: %w (tried: %s)", baseRef, headRef, err, strings.Join(triedBase, ", "))
		}
		return "", "", nil, nil, debug, err
	}
	moveFiles, err := moveFileList(baseCommit, headRef)
	if err != nil {
		return "", "", nil, nil, debug, err
	}
	return baseCommit, headRef, files, moveFiles, debug, nil
}

func processFile(path string, opts cliOptions, cfg *format.Config, baseDir *string, cfgLoaded *bool, resolver *configResolver, out io.Writer) (bool, error) {
	if !*cfgLoaded {
		var err error
		*cfg, *baseDir, err = resolver.Resolve(path)
		if err != nil {
			return false, err
		}
	}
	if format.IsExcluded(path, *baseDir, *cfg) {
		return false, nil
	}
	src, err := os.ReadFile(path)
	if err != nil {
		return false, err
	}
	formatted, err := format.FormatFile(path, src, *cfg)
	if err != nil {
		return false, err
	}
	changed := !bytesEqual(src, formatted)
	if opts.listFlag && changed {
		if _, err := fmt.Fprintln(out, path); err != nil {
			return false, err
		}
	}
	if opts.diffFlag && changed {
		df := unifiedDiff(path, src, formatted)
		if _, err := out.Write(df); err != nil {
			return false, err
		}
	}
	if opts.writeFlag && changed {
		// #nosec G306 G703 -- formatting output is not sensitive and path is from user input by design.
		if err := os.WriteFile(path, formatted, 0o644); err != nil {
			return false, err
		}
	}
	if err := outputFormatted(opts, formatted, out); err != nil {
		return false, err
	}
	return changed, nil
}

func processPath(path string, opts cliOptions, cfg *format.Config, baseDir *string, cfgLoaded *bool, resolver *configResolver, out io.Writer) (bool, error) {
	if path == "-" {
		return processStdin(opts, cfg, cfgLoaded, out)
	}
	return processFile(path, opts, cfg, baseDir, cfgLoaded, resolver, out)
}

func processStdin(opts cliOptions, cfg *format.Config, cfgLoaded *bool, out io.Writer) (bool, error) {
	stdinCfg := *cfg
	if !*cfgLoaded {
		stdinCfg = format.DefaultConfig()
		if err := format.ValidateConfig(stdinCfg); err != nil {
			return false, err
		}
	}
	src, err := io.ReadAll(os.Stdin)
	if err != nil {
		return false, err
	}
	formatted, err := format.FormatFile("stdin", src, stdinCfg)
	if err != nil {
		return false, err
	}
	changed := !bytesEqual(src, formatted)
	if opts.diffFlag && changed {
		df := unifiedDiff("stdin", src, formatted)
		if _, err := out.Write(df); err != nil {
			return false, err
		}
	}
	if opts.listFlag && changed {
		if _, err := fmt.Fprintln(out, "stdin"); err != nil {
			return false, err
		}
	}
	if err := outputFormatted(opts, formatted, out); err != nil {
		return false, err
	}
	return changed, nil
}

func refExists(ref string) bool {
	_, err := execGitCommand("rev-parse", "--verify", ref)
	return err == nil
}

func requireGitWorktree() error {
	// #nosec G204 G702 -- git command with controlled args.
	out, err := exec.CommandContext(context.Background(), "git", "rev-parse", "--is-inside-work-tree").CombinedOutput()
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
		return env, nil
	}
	if env := strings.TrimSpace(os.Getenv("GITHUB_BASE_REF")); env != "" {
		*tried = append(*tried, "GITHUB_BASE_REF="+env)
		return "origin/" + env, nil
	}
	candidates := []string{"origin/main", "origin/master", "main", "master"}
	for _, c := range candidates {
		*tried = append(*tried, c)
		if refExists(c) {
			return c, nil
		}
	}
	return "", errors.New("unable to resolve git base ref; provide -gh-base or set GITHUB_BASE_SHA/GITHUB_BASE_REF")
}

func resolveHeadRef(headRef string, tried *[]string) (string, error) {
	headRef = strings.TrimSpace(headRef)
	if headRef != "" {
		return headRef, nil
	}
	if env := strings.TrimSpace(os.Getenv("GITHUB_HEAD_SHA")); env != "" {
		*tried = append(*tried, "GITHUB_HEAD_SHA="+env)
		return env, nil
	}
	if env := strings.TrimSpace(os.Getenv("GITHUB_HEAD_REF")); env != "" {
		*tried = append(*tried, "GITHUB_HEAD_REF="+env)
		return "origin/" + env, nil
	}
	*tried = append(*tried, headRefValue)
	if refExists(headRefValue) {
		return headRefValue, nil
	}
	return "", errors.New("unable to resolve git head ref; provide -gh-head or set GITHUB_HEAD_SHA/GITHUB_HEAD_REF")
}

func runGHAnnotate(baseRef, headRef string) error {
	if err := requireGitWorktree(); err != nil {
		return err
	}
	baseCommit, resolvedHead, files, moveFiles, debug, err := prepareAnnotate(baseRef, headRef)
	if err != nil {
		return err
	}
	setNoticeHeadRef(resolvedHead)
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
		fileResult, err := annotateFile(path, baseCommit, resolvedHead, pkgCache, debug)
		if err != nil {
			return err
		}
		if fileResult.hasChanges {
			filesWithChanges++
		}
		if fileResult.annotatedDecls > 0 {
			filesAnnotated++
			annotatedDecls += fileResult.annotatedDecls
		}
	}
	if err := annotateMoves(moveFiles, baseCommit, resolver, debug); err != nil {
		return err
	}
	debugf(debug, "summary: files_scanned=%d files_with_changes=%d files_annotated=%d annotated_decls=%d", filesScanned, filesWithChanges, filesAnnotated, annotatedDecls)
	return nil
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

func bytesSplit(b []byte, sep byte) []string {
	if len(b) == 0 {
		return nil
	}
	var out []string
	start := 0
	for i := range b {
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

func debugf(enabled bool, format string, args ...any) {
	if !enabled {
		return
	}
	_, _ = fmt.Fprintf(os.Stderr, "gocan debug: "+format+"\n", args...)
}

type notice struct {
	File string
	Line int
	End  int
	Msg  string
	Kind noticeKind
	ID   string
}

type noticeKind int

const (
	noticeLayout noticeKind = iota
	noticeMovedFrom
	noticeMovedTo
)

const (
	headRefValue               = "HEAD"
	checkRunName               = "gocan layout-only"
	checkRunTitle              = "gocan layout-only annotations"
	checkRunConclusion         = "neutral"
	checkAnnotationsPerRequest = 50
	maxCheckAnnotations        = 1000
)

type noticeCollector struct {
	max             int
	summary         bool
	emitAnnotations bool
	emitChecks      bool
	checksHeadRef   string
	notices         []notice
}

var noticeSink *noticeCollector

func setNoticeCollector(maxNotices int, summary bool, emitAnnotations bool, emitChecks bool) {
	if maxNotices < 0 {
		maxNotices = 0
	}
	noticeSink = &noticeCollector{
		max:             maxNotices,
		summary:         summary,
		emitAnnotations: emitAnnotations,
		emitChecks:      emitChecks,
	}
}

func (c *noticeCollector) add(n notice) {
	c.notices = append(c.notices, n)
}

func setNoticeHeadRef(headRef string) {
	if noticeSink == nil {
		return
	}
	noticeSink.checksHeadRef = headRef
}

func flushNotices() {
	if noticeSink == nil {
		return
	}
	n := noticeSink
	logSuppressed := n.emitAnnotationNotices()
	if n.summary {
		if err := writeSummary(n.notices, logSuppressed); err != nil {
			_, _ = fmt.Fprintf(os.Stderr, "gocan: summary write failed: %v\n", err)
		}
	}
	if n.emitChecks {
		if err := publishCheckRun(n); err != nil {
			_, _ = fmt.Fprintf(os.Stderr, "gocan: checks publish failed: %v\n", err)
		}
	}
	noticeSink = nil
}

func (c *noticeCollector) emitAnnotationNotices() int {
	if !c.emitAnnotations {
		return 0
	}
	limit := c.max
	if limit > len(c.notices) || limit < 0 {
		limit = len(c.notices)
	}
	if len(c.notices) <= limit {
		sortNotices(c.notices)
		for i := 0; i < limit; i++ {
			printNotice(c.notices[i])
		}
		return 0
	}
	combined := combineNoticesByFile(c.notices)
	sortCombined(combined)
	if len(combined) > limit {
		dirCombined := combineNoticesByDir(c.notices)
		sortCombined(dirCombined)
		limit = min(limit, len(dirCombined))
		for i := 0; i < limit; i++ {
			printNotice(dirCombined[i])
		}
		return len(dirCombined) - limit
	}
	limit = min(limit, len(combined))
	for i := 0; i < limit; i++ {
		printNotice(combined[i])
	}
	return len(combined) - limit
}

func printNotice(n notice) {
	if n.End <= 0 || n.End < n.Line {
		n.End = n.Line
	}
	if n.Line <= 0 {
		fmt.Printf("::notice file=%s::%s\n", n.File, n.Msg)
		return
	}
	if n.End == n.Line {
		fmt.Printf("::notice file=%s,line=%d::%s\n", n.File, n.Line, n.Msg)
		return
	}
	fmt.Printf("::notice file=%s,line=%d,endLine=%d::%s\n", n.File, n.Line, n.End, n.Msg)
}

func writeSummary(notices []notice, suppressed int) error {
	path := strings.TrimSpace(os.Getenv("GITHUB_STEP_SUMMARY"))
	if path == "" {
		return nil
	}
	var b strings.Builder
	b.WriteString("### gocan layout-only annotations\n\n")
	if len(notices) == 0 {
		b.WriteString("No notices.\n")
		return appendFile(path, b.String())
	}
	grouped := groupNoticesByDir(notices)
	dirs := make([]string, 0, len(grouped))
	for dir := range grouped {
		dirs = append(dirs, dir)
	}
	slices.Sort(dirs)
	for _, dir := range dirs {
		b.WriteString("**Package:** `")
		b.WriteString(dir)
		b.WriteString("`\n")
		items := grouped[dir]
		sortNotices(items)
		for _, n := range items {
			b.WriteString("- ")
			b.WriteString(n.File)
			if n.Line > 0 {
				_, _ = fmt.Fprintf(&b, ":%d", n.Line)
				if n.End > n.Line {
					_, _ = fmt.Fprintf(&b, "-%d", n.End)
				}
			}
			b.WriteString(" — ")
			b.WriteString(n.Msg)
			b.WriteString("\n")
		}
		b.WriteString("\n")
	}
	if suppressed > 0 {
		_, _ = fmt.Fprintf(&b, "\n(%d additional notices suppressed in job log)\n", suppressed)
	}
	return appendFile(path, b.String())
}

func appendFile(path, content string) error {
	// #nosec G302 G703 -- summary file is a workflow artifact.
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer func() {
		if err := f.Close(); err != nil {
			log.Printf("gocan: failed to close summary file: %v", err)
		}
	}()
	_, err = f.WriteString(content)
	return err
}

func groupNoticesByDir(notices []notice) map[string][]notice {
	grouped := make(map[string][]notice, len(notices))
	for _, n := range notices {
		dir := filepath.Dir(n.File)
		if dir == "." {
			dir = "."
		}
		grouped[dir] = append(grouped[dir], n)
	}
	return grouped
}

func emitNoticeKind(path string, r hunkRange, msg string, kind noticeKind, id string) {
	if r.Start <= 0 {
		return
	}
	if r.End < r.Start {
		r.End = r.Start
	}
	n := notice{
		File: path,
		Line: r.Start,
		End:  r.End,
		Msg:  msg,
		Kind: kind,
		ID:   id,
	}
	if noticeSink == nil {
		printNotice(n)
		return
	}
	noticeSink.add(n)
}

func sortNotices(list []notice) {
	sort.SliceStable(list, func(i, j int) bool {
		pi := noticePriority(list[i].Kind)
		pj := noticePriority(list[j].Kind)
		if pi != pj {
			return pi < pj
		}
		if list[i].File != list[j].File {
			return list[i].File < list[j].File
		}
		if list[i].Line != list[j].Line {
			return list[i].Line < list[j].Line
		}
		return list[i].Msg < list[j].Msg
	})
}

func noticePriority(k noticeKind) int {
	switch k {
	case noticeMovedFrom:
		return 0
	case noticeLayout:
		return 1
	case noticeMovedTo:
		return 2
	default:
		return 3
	}
}

func combineNoticesByFile(list []notice) []notice {
	type bucket struct {
		file  string
		kinds map[noticeKind][]string
	}
	buckets := map[string]*bucket{}
	for _, n := range list {
		b := buckets[n.File]
		if b == nil {
			b = &bucket{
				file:  n.File,
				kinds: map[noticeKind][]string{},
			}
			buckets[n.File] = b
		}
		if n.ID != "" {
			b.kinds[n.Kind] = appendUnique(b.kinds[n.Kind], n.ID)
		} else {
			b.kinds[n.Kind] = appendUnique(b.kinds[n.Kind], n.Msg)
		}
	}
	out := make([]notice, 0, len(buckets))
	for _, b := range buckets {
		out = append(out, notice{
			File: b.file,
			Line: 0,
			End:  0,
			Msg:  composeCombinedMessage(b.kinds),
			Kind: noticeLayout,
		})
	}
	return out
}

func combineNoticesByDir(list []notice) []notice {
	type bucket struct {
		dir   string
		file  string
		kinds map[noticeKind][]string
	}
	buckets := map[string]*bucket{}
	for _, n := range list {
		dir := filepath.Dir(n.File)
		if dir == "." {
			dir = "."
		}
		b := buckets[dir]
		if b == nil {
			b = &bucket{
				dir:   dir,
				file:  n.File,
				kinds: map[noticeKind][]string{},
			}
			buckets[dir] = b
		}
		if n.File < b.file {
			b.file = n.File
		}
		if n.ID != "" {
			b.kinds[n.Kind] = appendUnique(b.kinds[n.Kind], n.ID)
		} else {
			b.kinds[n.Kind] = appendUnique(b.kinds[n.Kind], n.Msg)
		}
	}
	out := make([]notice, 0, len(buckets))
	for _, b := range buckets {
		out = append(out, notice{
			File: b.file,
			Line: 0,
			End:  0,
			Msg:  composeDirMessage(b.dir, b.kinds),
			Kind: noticeLayout,
		})
	}
	return out
}

func sortCombined(list []notice) {
	sort.SliceStable(list, func(i, j int) bool {
		if list[i].File != list[j].File {
			return list[i].File < list[j].File
		}
		return list[i].Msg < list[j].Msg
	})
}

func appendUnique(list []string, val string) []string {
	if slices.Contains(list, val) {
		return list
	}
	return append(list, val)
}

func composeCombinedMessage(kinds map[noticeKind][]string) string {
	var parts []string
	if s := formatIDList(kinds[noticeMovedFrom]); s != "" {
		parts = append(parts, "Moved from: "+s)
	}
	if s := formatIDList(kinds[noticeLayout]); s != "" {
		parts = append(parts, "Layout-only: "+s)
	}
	if s := formatIDList(kinds[noticeMovedTo]); s != "" {
		parts = append(parts, "Moved to: "+s)
	}
	if len(parts) == 0 {
		return "Layout-only changes"
	}
	return strings.Join(parts, "; ")
}

func composeDirMessage(dir string, kinds map[noticeKind][]string) string {
	return "Package " + dir + ": " + composeCombinedMessage(kinds)
}

func formatIDList(ids []string) string {
	if len(ids) == 0 {
		return ""
	}
	const maxItems = 3
	if len(ids) <= maxItems {
		return strings.Join(ids, ", ")
	}
	return strings.Join(ids[:maxItems], ", ") + fmt.Sprintf(", +%d more", len(ids)-maxItems)
}

type checkAnnotation struct {
	Path            string `json:"path"`
	StartLine       int    `json:"start_line"`
	EndLine         int    `json:"end_line"`
	AnnotationLevel string `json:"annotation_level"`
	Message         string `json:"message"`
}

type checkOutput struct {
	Title       string            `json:"title"`
	Summary     string            `json:"summary"`
	Annotations []checkAnnotation `json:"annotations,omitempty"`
}

type checkRunCreateRequest struct {
	Name       string      `json:"name"`
	HeadSHA    string      `json:"head_sha"`
	Status     string      `json:"status"`
	Conclusion string      `json:"conclusion"`
	Output     checkOutput `json:"output"`
}

type checkRunUpdateRequest struct {
	Output checkOutput `json:"output"`
}

type checkRunResponse struct {
	ID int64 `json:"id"`
}

func publishCheckRun(n *noticeCollector) error {
	repo, token, headSHA, err := checkRunEnv(n.checksHeadRef)
	if err != nil {
		return err
	}
	annotations, summary, suppressed := prepareCheckAnnotations(n.notices)
	id, err := createInitialCheckRun(repo, token, headSHA, annotations, summary)
	if err != nil {
		return err
	}
	debugf(debugEnabled(), "checks: created check run id=%d annotations=%d suppressed=%d", id, len(annotations), suppressed)
	return appendCheckRunAnnotations(repo, token, id, annotations, summary)
}

func checkRunEnv(headRef string) (repo string, token string, headSHA string, err error) {
	repo = strings.TrimSpace(os.Getenv("GITHUB_REPOSITORY"))
	if repo == "" {
		return "", "", "", errors.New("GITHUB_REPOSITORY is required for -gh-checks")
	}
	token = strings.TrimSpace(os.Getenv("GITHUB_TOKEN"))
	if token == "" {
		return "", "", "", errors.New("GITHUB_TOKEN is required for -gh-checks")
	}
	headRef = strings.TrimSpace(headRef)
	if headRef == "" {
		return "", "", "", errors.New("head ref is required for -gh-checks")
	}
	headSHA, err = resolveCommitSHA(headRef)
	if err != nil {
		return "", "", "", err
	}
	return repo, token, headSHA, nil
}

func prepareCheckAnnotations(notices []notice) ([]checkAnnotation, string, int) {
	annotations := buildCheckAnnotations(notices)
	suppressed := 0
	if len(annotations) > maxCheckAnnotations {
		suppressed = len(annotations) - maxCheckAnnotations
		annotations = annotations[:maxCheckAnnotations]
	}
	return annotations, checkSummary(len(notices), suppressed), suppressed
}

func createInitialCheckRun(repo, token, headSHA string, annotations []checkAnnotation, summary string) (int64, error) {
	firstBatch := annotations
	if len(firstBatch) > checkAnnotationsPerRequest {
		firstBatch = annotations[:checkAnnotationsPerRequest]
	}
	req := checkRunCreateRequest{
		Name:       checkRunName,
		HeadSHA:    headSHA,
		Status:     "completed",
		Conclusion: checkRunConclusion,
		Output: checkOutput{
			Title:       checkRunTitle,
			Summary:     summary,
			Annotations: firstBatch,
		},
	}
	return createCheckRun(repo, token, req)
}

func appendCheckRunAnnotations(repo, token string, id int64, annotations []checkAnnotation, summary string) error {
	for i := checkAnnotationsPerRequest; i < len(annotations); i += checkAnnotationsPerRequest {
		end := min(i+checkAnnotationsPerRequest, len(annotations))
		upd := checkRunUpdateRequest{
			Output: checkOutput{
				Title:       checkRunTitle,
				Summary:     summary,
				Annotations: annotations[i:end],
			},
		}
		if err := updateCheckRun(repo, token, id, upd); err != nil {
			return err
		}
	}
	return nil
}

func buildCheckAnnotations(list []notice) []checkAnnotation {
	out := make([]checkAnnotation, 0, len(list))
	for _, n := range list {
		line := n.Line
		if line <= 0 {
			line = 1
		}
		end := n.End
		if end <= 0 || end < line {
			end = line
		}
		out = append(out, checkAnnotation{
			Path:            n.File,
			StartLine:       line,
			EndLine:         end,
			AnnotationLevel: "notice",
			Message:         n.Msg,
		})
	}
	return out
}

func checkSummary(total int, suppressed int) string {
	if total == 0 {
		return "No layout-only notices."
	}
	summary := fmt.Sprintf("Layout-only notices: %d.", total)
	if suppressed > 0 {
		summary += fmt.Sprintf(" %d annotations suppressed to fit API limits.", suppressed)
	}
	summary += " See job summary for the full list."
	return summary
}

func createCheckRun(repo, token string, req checkRunCreateRequest) (int64, error) {
	url := fmt.Sprintf("https://api.github.com/repos/%s/check-runs", repo)
	var resp checkRunResponse
	if err := doGitHubRequest("POST", url, token, req, &resp); err != nil {
		return 0, err
	}
	if resp.ID == 0 {
		return 0, errors.New("check run id missing in response")
	}
	return resp.ID, nil
}

func updateCheckRun(repo, token string, id int64, req checkRunUpdateRequest) error {
	url := fmt.Sprintf("https://api.github.com/repos/%s/check-runs/%d", repo, id)
	return doGitHubRequest("PATCH", url, token, req, nil)
}

func doGitHubRequest(method, url, token string, req any, resp any) error {
	var body io.Reader
	if req != nil {
		payload, err := json.Marshal(req)
		if err != nil {
			return err
		}
		body = bytes.NewReader(payload)
	}
	// #nosec G704 -- URL is built from fixed GitHub API base.
	httpReq, err := http.NewRequestWithContext(context.Background(), method, url, body)
	if err != nil {
		return err
	}
	httpReq.Header.Set("Accept", "application/vnd.github+json")
	httpReq.Header.Set("Authorization", "Bearer "+token)
	httpReq.Header.Set("X-Github-Api-Version", "2022-11-28")
	if req != nil {
		httpReq.Header.Set("Content-Type", "application/json")
	}
	client := &http.Client{Timeout: 15 * time.Second}
	// #nosec G704 -- URL is built from fixed GitHub API base.
	httpResp, err := client.Do(httpReq)
	if err != nil {
		return err
	}
	defer func() {
		if err := httpResp.Body.Close(); err != nil {
			log.Printf("gocan: failed to close github response body: %v", err)
		}
	}()
	if httpResp.StatusCode < 200 || httpResp.StatusCode >= 300 {
		msg, readErr := io.ReadAll(httpResp.Body)
		if readErr != nil {
			return fmt.Errorf("github api %s failed: status %d (read error: %w)", method, httpResp.StatusCode, readErr)
		}
		if len(msg) > 0 {
			return fmt.Errorf("github api %s failed: %s", method, strings.TrimSpace(string(msg)))
		}
		return fmt.Errorf("github api %s failed: status %d", method, httpResp.StatusCode)
	}
	if resp == nil {
		return nil
	}
	return json.NewDecoder(httpResp.Body).Decode(resp)
}

func resolveCommitSHA(ref string) (string, error) {
	out, err := execGitCommand("rev-parse", ref)
	if err != nil {
		return "", err
	}
	sha := strings.TrimSpace(string(out))
	if sha == "" {
		return "", errors.New("resolved head sha is empty")
	}
	return sha, nil
}

func execGitCommand(args ...string) ([]byte, error) {
	// #nosec G204 G702 -- git commands with controlled args.
	cmd := exec.CommandContext(context.Background(), "git", args...)
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
	// #nosec G204 G702 -- git commands with controlled args.
	cmd := exec.CommandContext(context.Background(), "git", args...)
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

func fatal(err error) {
	var pathErr *os.PathError
	if errors.As(err, &pathErr) {
		_, _ = fmt.Fprintf(os.Stderr, "gocan: %v\n", pathErr)
		os.Exit(1)
	}

	_, _ = fmt.Fprintf(os.Stderr, "gocan: %v\n", err)
	os.Exit(1)
}

func gitShow(ref, path string) ([]byte, error) {
	arg := fmt.Sprintf("%s:%s", ref, path)
	return execGitCommand("show", arg)
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
	out, err := execGitCommand("ls-tree", "-r", "--name-only", commit, "--", dir)
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

func outputFormatted(opts cliOptions, formatted []byte, out io.Writer) error {
	if opts.writeFlag || opts.listFlag || opts.diffFlag {
		return nil
	}
	if _, err := out.Write(formatted); err != nil {
		return err
	}
	if len(formatted) == 0 || formatted[len(formatted)-1] != '\n' {
		if _, err := out.Write([]byte("\n")); err != nil {
			return err
		}
	}
	return nil
}

func packageKey(dir, pkg string) string {
	return dir + "||" + pkg
}

func parseFlags() (cliOptions, []string) {
	var opts cliOptions
	flag.BoolVar(&opts.writeFlag, "w", false, "write result to (source) file instead of stdout")
	flag.BoolVar(&opts.listFlag, "l", false, "list files whose formatting differs from gocan's")
	flag.BoolVar(&opts.diffFlag, "d", false, "display diffs instead of rewriting files")
	flag.BoolVar(&opts.checkFlag, "check", false, "exit non-zero if any file is not formatted")
	flag.StringVar(&opts.diffFile, "diff", "", "git diff file for changes (optional, used with -denoise)")
	flag.StringVar(&opts.parentCommit, "parent-commit", "", "git parent commit for diff base (optional, used with -denoise)")
	flag.StringVar(&opts.configPath, "config", "", "path to JSON config file")
	flag.BoolVar(&opts.ghAnnotate, "gh-annotate", false, "emit GitHub Actions annotations for layout-only changes")
	flag.BoolVar(&opts.ghChecks, "gh-checks", false, "publish GitHub Checks API annotations for layout-only changes")
	flag.StringVar(&opts.ghHead, "gh-head", "", "git head ref/sha for -gh-annotate (defaults to GITHUB_HEAD_SHA/GITHUB_HEAD_REF, otherwise HEAD)")
	flag.StringVar(&opts.ghBase, "gh-base", "", "git base ref/sha for -gh-annotate (defaults to GITHUB_BASE_SHA/GITHUB_BASE_REF if set)")
	flag.IntVar(&opts.ghMaxNotices, "gh-max-notices", 10, "max GitHub Actions notices to emit")
	flag.BoolVar(&opts.ghSkipSummary, "gh-skip-summary", false, "skip writing full notice list to GITHUB_STEP_SUMMARY")
	flag.Parse()
	return opts, flag.Args()
}

func unifiedDiff(path string, a, b []byte) []byte {
	return diff.Diff(path, a, path, b)
}

type annotationResult struct {
	hasChanges     bool
	annotatedDecls int
}

type cliOptions struct {
	writeFlag     bool
	listFlag      bool
	diffFlag      bool
	checkFlag     bool
	diffFile      string
	parentCommit  string
	configPath    string
	ghAnnotate    bool
	ghChecks      bool
	ghHead        string
	ghBase        string
	ghMaxNotices  int
	ghSkipSummary bool
}

type declOccurrence struct {
	declSpan
	File   string
	Pkg    string
	PkgKey string
}

type declSpan struct {
	Fingerprint string
	Kind        string
	Name        string
	DisplayName string
	StartLine   int
	EndLine     int
}

type hunkRange struct {
	Start int
	End   int
}

type packageCache struct {
	baseCommit string
	resolver   *configResolver
	contexts   map[string]*packageContext
	fileToKey  map[string]string
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

func (c *packageCache) ensureContext(dir, pkg string) *packageContext {
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
	return ctx
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

	if err := c.loadHeadFiles(dir, headFiles); err != nil {
		return err
	}
	if err := c.loadBaseFiles(dir, baseFiles); err != nil {
		return err
	}

	return nil
}

func (c *packageCache) headSource(path string) []byte {
	if key, ok := c.fileToKey[path]; ok {
		if ctx, ok := c.contexts[key]; ok {
			return ctx.headSrcByFile[path]
		}
	}
	if err := c.ensureDir(filepath.Dir(path)); err != nil {
		return nil
	}
	if key, ok := c.fileToKey[path]; ok {
		if ctx, ok := c.contexts[key]; ok {
			return ctx.headSrcByFile[path]
		}
	}
	return nil
}

func (c *packageCache) loadBaseFiles(dir string, baseFiles []string) error {
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
		ctx := c.ensureContext(dir, pkg)
		for _, d := range decls {
			ctx.baseCounts[d.Fingerprint]++
		}
	}
	return nil
}

func (c *packageCache) loadHeadFiles(dir string, headFiles []string) error {
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
		ctx := c.ensureContext(dir, pkg)
		c.fileToKey[path] = packageKey(dir, pkg)
		ctx.headDeclsByFile[path] = decls
		ctx.headSrcByFile[path] = src
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
	if err := c.ensureDir(filepath.Dir(path)); err != nil {
		return nil
	}
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

type packageContext struct {
	baseCounts        map[string]int
	headDeclsByFile   map[string][]declSpan
	headSrcByFile     map[string][]byte
	unchangedByFile   map[string][]declSpan
	unchangedComputed bool
}
