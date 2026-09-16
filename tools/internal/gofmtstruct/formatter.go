// Package gofmtstruct provides RepoSuite Relay's structural Go
// formatting rule: multi-field keyed struct literals are written vertically.
// It does not rewrite maps, arrays, slices, or unkeyed literals.
package gofmtstruct

import (
	"bytes"
	"fmt"
	"go/ast"
	"go/format"
	"go/importer"
	"go/parser"
	"go/scanner"
	"go/token"
	"go/types"
	"sort"
)

type edit struct {
	offset int
	text   string
}

// FormatSource rewrites multi-field keyed struct literals vertically and then
// applies gofmt. It does not rewrite maps, arrays, slices, or unkeyed literals.
func FormatSource(filename string, source []byte) ([]byte, error) {
	formatted, err := FormatPackage(map[string][]byte{filename: source})
	if err != nil {
		return nil, err
	}
	return formatted[filename], nil
}

// FormatPackage formats the supplied Go package files with one type index.
// Keeping the package index here lets a file be classified using declarations
// from its sibling files and imported package export data.
func FormatPackage(sources map[string][]byte) (map[string][]byte, error) {
	fset := token.NewFileSet()
	parsed := make(map[string]*ast.File, len(sources))
	packages := make(map[string][]*ast.File)
	for filename, source := range sources {
		file, err := parser.ParseFile(fset, filename, source, parser.ParseComments)
		if err != nil {
			return nil, err
		}
		parsed[filename] = file
		packages[file.Name.Name] = append(packages[file.Name.Name], file)
	}
	formatted := make(map[string][]byte, len(sources))
	for packageName, files := range packages {
		resolver := newTypeResolver(fset, packageName, files)
		for filename, file := range parsed {
			if file.Name.Name != packageName {
				continue
			}
			formattedSource, err := formatFile(fset, file, sources[filename], resolver)
			if err != nil {
				return nil, err
			}
			formatted[filename] = formattedSource
		}
	}
	return formatted, nil
}

type typeResolver struct {
	types    map[ast.Expr]types.TypeAndValue
	declared map[string]bool
}

func newTypeResolver(fset *token.FileSet, packageName string, files []*ast.File) *typeResolver {
	info := &types.Info{Types: make(map[ast.Expr]types.TypeAndValue)}
	checker := types.Config{
		Importer: importer.Default(),
		Error:    func(error) {},
	}
	_, _ = checker.Check(packageName, fset, files, info)
	return &typeResolver{
		types:    info.Types,
		declared: declaredTypeKinds(files),
	}
}

func formatFile(fset *token.FileSet, file *ast.File, source []byte, resolver *typeResolver) ([]byte, error) {
	commas, err := commaOffsets(fset, file.Pos(), source)
	if err != nil {
		return nil, err
	}
	edits := make([]edit, 0)
	stack := make([]ast.Node, 0)
	ast.Inspect(file, func(node ast.Node) bool {
		if node == nil {
			stack = stack[:len(stack)-1]
			return true
		}
		nestedComposite := false
		for _, parent := range stack {
			if _, ok := parent.(*ast.CompositeLit); ok {
				nestedComposite = true
				break
			}
		}
		stack = append(stack, node)
		literal, ok := node.(*ast.CompositeLit)
		if !ok || !isKeyedStructLiteral(literal, resolver) || len(literal.Elts) == 0 || (len(literal.Elts) < 2 && !nestedComposite) {
			return true
		}
		for _, element := range literal.Elts {
			if _, ok := element.(*ast.KeyValueExpr); !ok {
				return true
			}
		}

		open := fset.PositionFor(literal.Lbrace, false).Offset
		close := fset.PositionFor(literal.Rbrace, false).Offset
		first := fset.PositionFor(literal.Elts[0].Pos(), false).Offset
		if !containsNewline(source[open+1 : first]) {
			edits = append(edits, edit{
				offset: open + 1,
				text:   "\n",
			})
		}
		closeHasNewline := false
		for index, element := range literal.Elts {
			end := fset.PositionFor(element.End(), false).Offset
			limit := close
			if index+1 < len(literal.Elts) {
				limit = fset.PositionFor(literal.Elts[index+1].Pos(), false).Offset
			}
			comma := firstCommaAfter(commas, end, limit)
			if comma < 0 {
				if index+1 < len(literal.Elts) {
					return true
				}
				edits = append(edits, edit{
					offset: close,
					text:   ",\n",
				})
				closeHasNewline = true
				continue
			}
			if !containsNewline(source[comma+1 : limit]) {
				edits = append(edits, edit{
					offset: comma + 1,
					text:   "\n",
				})
			}
		}
		if !closeHasNewline && !containsNewline(source[fset.PositionFor(literal.Elts[len(literal.Elts)-1].End(), false).Offset:close]) {
			edits = append(edits, edit{
				offset: close,
				text:   "\n",
			})
		}
		return true
	})

	if len(edits) > 0 {
		sort.SliceStable(edits, func(i, j int) bool { return edits[i].offset < edits[j].offset })
		var transformed bytes.Buffer
		transformed.Grow(len(source) + len(edits))
		cursor := 0
		for _, change := range edits {
			if change.offset < cursor || change.offset > len(source) {
				return nil, fmt.Errorf("invalid formatter edit offset %d", change.offset)
			}
			transformed.Write(source[cursor:change.offset])
			transformed.WriteString(change.text)
			cursor = change.offset
		}
		transformed.Write(source[cursor:])
		source = transformed.Bytes()
	}
	return format.Source(source)
}

func containsNewline(source []byte) bool { return bytes.IndexByte(source, '\n') >= 0 }

func commaOffsets(fset *token.FileSet, first token.Pos, source []byte) ([]int, error) {
	file := fset.File(first)
	if file == nil {
		return nil, fmt.Errorf("formatter token file is unavailable")
	}
	var scan scanner.Scanner
	scan.Init(file, source, nil, scanner.ScanComments)
	commas := make([]int, 0)
	for {
		position, tok, _ := scan.Scan()
		if tok == token.EOF {
			break
		}
		if tok == token.COMMA {
			commas = append(commas, file.Offset(position))
		}
	}
	return commas, nil
}

func firstCommaAfter(commas []int, start, limit int) int {
	for _, comma := range commas {
		if comma >= start && comma < limit {
			return comma
		}
	}
	return -1
}

func declaredTypeKinds(files []*ast.File) map[string]bool {
	kinds := make(map[string]bool)
	for _, file := range files {
		for _, declaration := range file.Decls {
			gen, ok := declaration.(*ast.GenDecl)
			if !ok || gen.Tok != token.TYPE {
				continue
			}
			for _, specification := range gen.Specs {
				typeSpec, ok := specification.(*ast.TypeSpec)
				if !ok {
					continue
				}
				if _, ok := typeSpec.Type.(*ast.StructType); ok {
					kinds[typeSpec.Name.Name] = true
				} else if _, exists := kinds[typeSpec.Name.Name]; !exists {
					kinds[typeSpec.Name.Name] = false
				}
			}
		}
	}
	return kinds
}

func isKeyedStructLiteral(literal *ast.CompositeLit, resolver *typeResolver) bool {
	switch literal.Type.(type) {
	case *ast.MapType, *ast.ArrayType:
		return false
	case *ast.StructType:
		return true
	}
	if typed, ok := resolver.types[literal.Type]; ok && typed.Type != nil {
		_, isStruct := typed.Type.Underlying().(*types.Struct)
		return isStruct
	}
	if ident, ok := literal.Type.(*ast.Ident); ok {
		kind, known := resolver.declared[ident.Name]
		return known && kind
	}
	return false
}
