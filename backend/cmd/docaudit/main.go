// Command docaudit checks that every Go declaration in the backend has a doc
// comment that starts with its name, and that every package has a package
// comment. It prints each problem and exits non-zero if there are any.
//
//	cd backend && go run ./cmd/docaudit
package main

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// main audits the current directory tree.
func main() {
	problems, err := audit(".")
	if err != nil {
		fmt.Fprintln(os.Stderr, "docaudit:", err)
		os.Exit(2)
	}
	for _, p := range problems {
		fmt.Println(p)
	}
	if len(problems) > 0 {
		fmt.Printf("%d problems\n", len(problems))
		os.Exit(1)
	}
	fmt.Println("docaudit: ok")
}

// audit returns one line per missing or misworded doc comment under root.
func audit(root string) ([]string, error) {
	fset := token.NewFileSet()
	var problems []string
	pkgDoc := map[string]bool{} // directory → has a package comment

	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".go") {
			return err
		}
		f, err := parser.ParseFile(fset, path, nil, parser.ParseComments)
		if err != nil {
			return err
		}
		dir := filepath.Dir(path)
		if _, seen := pkgDoc[dir]; !seen {
			pkgDoc[dir] = false
		}
		if f.Doc != nil && !strings.HasSuffix(path, "_test.go") {
			pkgDoc[dir] = true
		}
		report := func(pos token.Pos, msg string) {
			problems = append(problems, fmt.Sprintf("%s: %s", fset.Position(pos), msg))
		}
		for _, decl := range f.Decls {
			checkDecl(decl, report)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	var dirs []string
	for dir, ok := range pkgDoc {
		if !ok {
			dirs = append(dirs, dir)
		}
	}
	sort.Strings(dirs)
	for _, dir := range dirs {
		problems = append(problems, dir+": no package comment (add doc.go)")
	}
	return problems, nil
}

// checkDecl reports a declaration whose doc comment is missing or doesn't
// start with its name.
func checkDecl(decl ast.Decl, report func(token.Pos, string)) {
	switch d := decl.(type) {
	case *ast.FuncDecl:
		check(d.Doc, d.Name.Name, d.Pos(), "func", report)
	case *ast.GenDecl:
		if d.Tok == token.IMPORT {
			return
		}
		for _, spec := range d.Specs {
			doc := d.Doc
			var name string
			var pos token.Pos
			switch s := spec.(type) {
			case *ast.TypeSpec:
				name, pos = s.Name.Name, s.Pos()
				if s.Doc != nil {
					doc = s.Doc
				}
			case *ast.ValueSpec:
				name, pos = s.Names[0].Name, s.Pos()
				if s.Doc != nil {
					doc = s.Doc
				} else if d.Lparen.IsValid() {
					doc = nil // in a group, each item needs its own comment
				}
			}
			if name == "_" {
				continue
			}
			check(doc, name, pos, d.Tok.String(), report)
		}
	}
}

// check reports doc if it's missing or doesn't start with name.
func check(doc *ast.CommentGroup, name string, pos token.Pos, kind string, report func(token.Pos, string)) {
	if doc == nil || strings.TrimSpace(doc.Text()) == "" {
		report(pos, fmt.Sprintf("%s %s has no doc comment", kind, name))
		return
	}
	text := doc.Text()
	for _, article := range []string{"", "A ", "An ", "The "} {
		rest, ok := strings.CutPrefix(text, article+name)
		if ok && (rest == "" || strings.ContainsRune(" ,:'.", rune(rest[0]))) {
			return
		}
	}
	report(pos, fmt.Sprintf("%s %s: doc comment should start with %q", kind, name, name))
}
