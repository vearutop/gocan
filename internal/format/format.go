package format

import (
	"bytes"
	"fmt"
	"go/ast"
	"go/format"
	"go/parser"
	"go/printer"
	"go/token"
	"path/filepath"
	"sort"
	"strings"
)

func CanonicalizePath(path string) string {
	return filepath.Clean(path)
}

func FormatFile(path string, src []byte, cfg Config) ([]byte, error) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, path, src, parser.ParseComments)
	if err != nil {
		return nil, err
	}

	imports := make([]ast.Decl, 0, len(file.Decls))
	others := make([]ast.Decl, 0, len(file.Decls))

	for _, decl := range file.Decls {
		if gen, ok := decl.(*ast.GenDecl); ok && gen.Tok == token.IMPORT {
			imports = append(imports, decl)
			continue
		}
		others = append(others, decl)
	}

	othersOrig := append([]ast.Decl(nil), others...)
	infos := make([]declInfo, len(others))
	for i, decl := range others {
		kind, name, exported := classifyDecl(decl)
		if kind == "func" && name == "main" && file.Name != nil && file.Name.Name == "main" {
			kind = "packageMainFunc"
		}
		orderIndex := orderIndexFor(cfg, kind, exported)
		tie, _ := declText(fset, decl)

		key := declKey{
			OrderIndex: orderIndex,
			Name:       strings.ToLower(name),
			Tie:        tie,
			OrigIndex:  i,
		}
		if kind == "method" {
			if recvName, recvExported, ok := receiverTypeInfo(decl); ok {
				key.OrderIndex = orderIndexFor(cfg, "type", recvExported)
				key.Name = strings.ToLower(recvName)
				key.SubOrder = 1
				key.MethodGroup = receiverMethodGroup(cfg, recvExported, exported)
				key.MethodName = strings.ToLower(name)
			}
		} else if kind == "type" {
			key.SubOrder = 0
		}
		isCallable := isCallableDecl(decl)
		isHelper := kind == "func" && !exported && isCallable
		infos[i] = declInfo{
			Decl:       decl,
			Key:        key,
			Kind:       kind,
			Exported:   exported,
			Name:       name,
			IsHelper:   isHelper,
			IsCallable: isCallable,
		}
	}

	if cfg.HelperAttachment {
		children, _, _ := helperAttachmentData(infos)
		for i := range infos {
			if infos[i].IsCallable {
				if _, ok := children[infos[i].Name]; ok {
					infos[i].Key.HasAttached = true
				}
			}
		}
	}

	sort.SliceStable(infos, func(i, j int) bool {
		ki := infos[i].Key
		kj := infos[j].Key
		if ki.OrderIndex != kj.OrderIndex {
			return ki.OrderIndex < kj.OrderIndex
		}
		if ki.HasAttached != kj.HasAttached {
			return ki.HasAttached && !kj.HasAttached
		}
		if ki.Name != kj.Name {
			return ki.Name < kj.Name
		}
		if ki.SubOrder != kj.SubOrder {
			return ki.SubOrder < kj.SubOrder
		}
		if ki.MethodGroup != kj.MethodGroup {
			return ki.MethodGroup < kj.MethodGroup
		}
		if ki.MethodName != kj.MethodName {
			return ki.MethodName < kj.MethodName
		}
		if ki.Tie != kj.Tie {
			return ki.Tie < kj.Tie
		}
		return ki.OrigIndex < kj.OrigIndex
	})

	if cfg.HelperAttachment {
		infos = attachHelpers(infos)
	}

	file.Decls = file.Decls[:0]
	file.Decls = append(file.Decls, imports...)
	for _, info := range infos {
		file.Decls = append(file.Decls, info.Decl)
	}

	return renderFile(fset, file, imports, infos, othersOrig)
}

func ValidateConfig(cfg Config) error {
	validKinds := map[string]bool{
		"const":           true,
		"var":             true,
		"type":            true,
		"func":            true,
		"receiver":        true,
		"packageMainFunc": true,
		"constructor":     true,
		"unknown":         true,
	}
	for _, rule := range cfg.Order {
		if !validKinds[rule.Kind] {
			return fmt.Errorf("unknown kind in config: %s", rule.Kind)
		}
	}
	return nil
}

func adjustCommentGroups(base token.Pos, groups []*ast.CommentGroup) []*ast.CommentGroup {
	if len(groups) == 0 {
		return nil
	}
	if base > 1 {
		base--
	}
	out := make([]*ast.CommentGroup, len(groups))
	for i, g := range groups {
		list := make([]*ast.Comment, len(g.List))
		for j, c := range g.List {
			nc := *c
			nc.Slash = base
			list[j] = &nc
		}
		out[i] = &ast.CommentGroup{List: list}
	}
	return out
}

func attachHelpers(infos []declInfo) []declInfo {
	children, attachedToRoot, helpers := helperAttachmentData(infos)

	// sort children by helper name (case-insensitive), then tie, then original index
	for parent := range children {
		list := children[parent]
		sort.SliceStable(list, func(i, j int) bool {
			ai := infos[helpers[list[i]]]
			bi := infos[helpers[list[j]]]
			an := strings.ToLower(ai.Name)
			bn := strings.ToLower(bi.Name)
			if an != bn {
				return an < bn
			}
			if ai.Key.Tie != bi.Key.Tie {
				return ai.Key.Tie < bi.Key.Tie
			}
			return ai.Key.OrigIndex < bi.Key.OrigIndex
		})
		children[parent] = list
	}

	skipped := map[string]struct{}{}
	for helper := range attachedToRoot {
		skipped[helper] = struct{}{}
	}

	result := make([]declInfo, 0, len(infos))
	for _, info := range infos {
		if _, ok := skipped[info.Name]; ok && info.IsHelper {
			continue
		}
		result = append(result, info)
		if info.IsCallable {
			if list, ok := children[info.Name]; ok {
				result = append(result, expandHelpers(list, children, helpers, infos)...)
			}
		}
	}
	return result
}

func helperAttachmentData(infos []declInfo) (map[string][]string, map[string]struct{}, map[string]int) {
	helpers := map[string]int{}
	callers := map[string]int{}
	for i, info := range infos {
		if info.IsHelper {
			helpers[info.Name] = i
		}
		if info.IsCallable {
			callers[info.Name] = i
		}
	}

	parentSets := map[string]map[string]struct{}{}
	for _, info := range infos {
		fn, ok := info.Decl.(*ast.FuncDecl)
		if !ok {
			continue
		}
		calls := collectCalls(fn.Body)
		if len(calls) == 0 {
			continue
		}
		for _, callee := range calls {
			if _, ok := helpers[callee]; !ok {
				continue
			}
			set := parentSets[callee]
			if set == nil {
				set = map[string]struct{}{}
				parentSets[callee] = set
			}
			set[info.Name] = struct{}{}
		}
	}

	attached := map[string]string{} // helper -> parent (direct)
	for helper, parents := range parentSets {
		if len(parents) != 1 {
			continue
		}
		for parent := range parents {
			helperIdx := helpers[helper]
			parentIdx, ok := callers[parent]
			if !ok {
				continue
			}
			if infos[helperIdx].Key.OrderIndex != infos[parentIdx].Key.OrderIndex {
				continue
			}
			attached[helper] = parent
		}
	}

	// Build helper tree rooted at the first non-helper ancestor.
	children := map[string][]string{} // parent -> helpers
	attachedToRoot := map[string]struct{}{}
	for helper := range attached {
		root := resolveRootParent(helper, attached, helpers, infos, callers)
		if root == "" {
			continue
		}
		children[root] = append(children[root], helper)
		attachedToRoot[helper] = struct{}{}
	}
	return children, attachedToRoot, helpers
}

func resolveRootParent(helper string, attached map[string]string, helpers map[string]int, infos []declInfo, callers map[string]int) string {
	seen := map[string]struct{}{}
	cur := helper
	for {
		if _, ok := seen[cur]; ok {
			return ""
		}
		seen[cur] = struct{}{}
		parent, ok := attached[cur]
		if !ok {
			return ""
		}
		if _, ok := callers[parent]; !ok {
			return ""
		}
		if _, ok := attached[parent]; !ok {
			return parent
		}
		cur = parent
	}
}

func expandHelpers(list []string, children map[string][]string, helpers map[string]int, infos []declInfo) []declInfo {
	var out []declInfo
	for _, name := range list {
		out = append(out, infos[helpers[name]])
		if kids, ok := children[name]; ok {
			out = append(out, expandHelpers(kids, children, helpers, infos)...)
		}
	}
	return out
}

func collectCalls(body *ast.BlockStmt) []string {
	if body == nil {
		return nil
	}
	var calls []string
	ast.Inspect(body, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok {
			return true
		}
		ident, ok := call.Fun.(*ast.Ident)
		if !ok {
			return true
		}
		calls = append(calls, ident.Name)
		return true
	})
	return calls
}

func classifyDecl(decl ast.Decl) (kind, name string, exported bool) {
	switch d := decl.(type) {
	case *ast.GenDecl:
		switch d.Tok {
		case token.CONST:
			kind = "const"
		case token.VAR:
			kind = "var"
		case token.TYPE:
			kind = "type"
		default:
			kind = strings.ToLower(d.Tok.String())
		}

		name = genDeclPrimaryName(d)
		exported = ast.IsExported(name)
		return kind, name, exported
	case *ast.FuncDecl:
		name = d.Name.Name
		exported = ast.IsExported(name)
		if d.Recv != nil {
			return "method", name, exported
		}
		if strings.HasPrefix(name, "New") {
			return "constructor", name, exported
		}
		return "func", name, exported
	default:
		return "unknown", "", false
	}
}

func genDeclPrimaryName(d *ast.GenDecl) string {
	if len(d.Specs) == 0 {
		return ""
	}
	switch spec := d.Specs[0].(type) {
	case *ast.ValueSpec:
		if len(spec.Names) == 0 {
			return ""
		}
		return spec.Names[0].Name
	case *ast.TypeSpec:
		return spec.Name.Name
	default:
		return ""
	}
}

func declText(fset *token.FileSet, decl ast.Decl) (string, error) {
	var buf bytes.Buffer
	if err := format.Node(&buf, fset, decl); err != nil {
		return "", err
	}
	return buf.String(), nil
}

func isCallableDecl(decl ast.Decl) bool {
	fn, ok := decl.(*ast.FuncDecl)
	return ok && fn.Recv == nil
}

func orderIndexFor(cfg Config, kind string, exported bool) int {
	for i, rule := range cfg.Order {
		if rule.Kind != kind {
			continue
		}
		if kind == "packageMainFunc" || rule.Exported == exported {
			return i
		}
	}
	if kind == "packageMainFunc" {
		return orderIndexFor(cfg, "func", false)
	}
	return len(cfg.Order) + 1
}

func receiverMethodGroup(cfg Config, receiverExported, methodExported bool) int {
	for i, rule := range cfg.Order {
		if rule.Kind != "receiver" {
			continue
		}
		if rule.Exported == receiverExported && rule.ExportedMethod == methodExported {
			return i
		}
	}
	if methodExported {
		return 0
	}
	return 1
}

func receiverTypeInfo(decl ast.Decl) (string, bool, bool) {
	fn, ok := decl.(*ast.FuncDecl)
	if !ok || fn.Recv == nil || len(fn.Recv.List) == 0 {
		return "", false, false
	}
	t := fn.Recv.List[0].Type
	switch v := t.(type) {
	case *ast.Ident:
		return v.Name, ast.IsExported(v.Name), true
	case *ast.StarExpr:
		if id, ok := v.X.(*ast.Ident); ok {
			return id.Name, ast.IsExported(id.Name), true
		}
	}
	return "", false, false
}

func renderFile(fset *token.FileSet, file *ast.File, imports []ast.Decl, infos []declInfo, othersOrig []ast.Decl) ([]byte, error) {
	commentIndex := buildCommentIndex(file, imports, othersOrig)

	importFile := &ast.File{
		Name:    file.Name,
		Package: file.Package,
		Doc:     file.Doc,
		Decls:   imports,
	}
	importFile.Comments = append([]*ast.CommentGroup{}, commentIndex.headerComments...)
	importFile.Comments = append(importFile.Comments, commentIndex.importComments...)

	var out bytes.Buffer
	if err := format.Node(&out, fset, importFile); err != nil {
		return nil, err
	}

	if len(infos) > 0 {
		if out.Len() == 0 || out.Bytes()[out.Len()-1] != '\n' {
			out.WriteByte('\n')
		}
		out.WriteByte('\n')
	}

	pc := &printer.Config{Mode: printer.UseSpaces | printer.TabIndent, Tabwidth: 8}
	for i, info := range infos {
		comments := commentIndex.commentsForDecl(info.Decl)
		if i == len(infos)-1 {
			comments = append(comments, adjustCommentGroups(info.Decl.End(), commentIndex.trailing)...)
		}
		var buf bytes.Buffer
		if err := pc.Fprint(&buf, fset, &printer.CommentedNode{Node: info.Decl, Comments: comments}); err != nil {
			return nil, err
		}
		out.Write(buf.Bytes())
		if i != len(infos)-1 {
			if out.Len() == 0 || out.Bytes()[out.Len()-1] != '\n' {
				out.WriteByte('\n')
			}
			out.WriteByte('\n')
		}
	}

	if out.Len() == 0 || out.Bytes()[out.Len()-1] != '\n' {
		out.WriteByte('\n')
	}
	return out.Bytes(), nil
}

func buildCommentIndex(file *ast.File, imports []ast.Decl, othersOrig []ast.Decl) commentIndex {
	idx := commentIndex{
		inner:   make(map[ast.Decl][]*ast.CommentGroup),
		leading: make(map[ast.Decl][]*ast.CommentGroup),
	}
	if file.Doc != nil {
		idx.headerComments = append(idx.headerComments, file.Doc)
	}

	importSpans := declSpans(imports)
	otherSpans := declSpans(othersOrig)

	for _, cg := range file.Comments {
		if file.Doc != nil && cg == file.Doc {
			continue
		}
		if cg.End() < file.Package {
			idx.headerComments = append(idx.headerComments, cg)
			continue
		}
		if decl := findContaining(importSpans, cg); decl != nil {
			idx.importComments = append(idx.importComments, cg)
			continue
		}
		if decl := findContaining(otherSpans, cg); decl != nil {
			idx.inner[decl] = append(idx.inner[decl], cg)
			continue
		}
		// Free comment: attach to next non-import decl if any.
		if next := findNextDecl(otherSpans, cg); next != nil {
			idx.leading[next] = append(idx.leading[next], cg)
			continue
		}
		idx.trailing = append(idx.trailing, cg)
	}

	return idx
}

type commentIndex struct {
	importComments []*ast.CommentGroup
	headerComments []*ast.CommentGroup
	inner          map[ast.Decl][]*ast.CommentGroup
	leading        map[ast.Decl][]*ast.CommentGroup
	trailing       []*ast.CommentGroup
}

func declSpans(decls []ast.Decl) []span {
	spans := make([]span, 0, len(decls))
	for _, decl := range decls {
		spans = append(spans, span{decl: decl, pos: decl.Pos(), end: decl.End()})
	}
	return spans
}

func findContaining(spans []span, cg *ast.CommentGroup) ast.Decl {
	for _, sp := range spans {
		if cg.Pos() >= sp.pos && cg.End() <= sp.end {
			return sp.decl
		}
	}
	return nil
}

func findNextDecl(spans []span, cg *ast.CommentGroup) ast.Decl {
	for _, sp := range spans {
		if cg.End() < sp.pos {
			return sp.decl
		}
	}
	return nil
}

func (c commentIndex) commentsForDecl(decl ast.Decl) []*ast.CommentGroup {
	groups := adjustCommentGroups(decl.Pos(), c.leading[decl])
	groups = append(groups, c.inner[decl]...)
	return groups
}

type declInfo struct {
	Decl       ast.Decl
	Key        declKey
	Kind       string
	Exported   bool
	Name       string
	IsHelper   bool
	IsCallable bool
}

type declKey struct {
	OrderIndex  int
	Name        string
	SubOrder    int
	MethodGroup int
	MethodName  string
	HasAttached bool
	Tie         string
	OrigIndex   int
}

type span struct {
	decl ast.Decl
	pos  token.Pos
	end  token.Pos
}
