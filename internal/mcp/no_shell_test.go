package mcp

// no_shell_test.go — the audit that no tool builds a shell command.
//
// # Why this is a test and not a review note
//
// Every exec in this package and in internal/generator already passes an argument
// array, so no shell parses any caller-supplied value and `; rm -rf /` in a service
// name arrives as one literal argument. That was established by reading the code.
//
// Reading the code establishes it for the code that exists. The failure this guards
// against is the next `exec.Command("sh", "-c", "docker build "+dir)` — written because
// it is shorter, or because a pipeline was needed — and the property it destroys is not
// local to that line: one such call makes every validated identifier in
// docker_names.go irrelevant, because the values were only ever safe as argv elements.
//
// So the rule is checked mechanically, over both packages' real sources, at the level
// the risk lives: which program is being started.

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"strings"
	"testing"
)

// shellPrograms are the programs that interpret their argument as a command.
//
// Windows spellings included, because the risk is not platform-specific and a
// `cmd /c` reaching a caller-supplied string is the same hole as `sh -c`.
var shellPrograms = map[string]bool{
	"sh": true, "bash": true, "zsh": true, "dash": true, "ksh": true, "csh": true,
	"/bin/sh": true, "/bin/bash": true, "/usr/bin/sh": true, "/usr/bin/bash": true,
	"cmd": true, "cmd.exe": true, "command.com": true,
	"powershell": true, "powershell.exe": true, "pwsh": true, "pwsh.exe": true,
	"env": true, "eval": true, "xargs": true,
}

// TestNoToolStartsAShell walks the two packages that execute anything.
//
// It flags a call to exec.Command or exec.CommandContext whose program argument is a
// shell, and separately flags a "-c" or "/c" argument anywhere in such a call — the
// second catches the case where the program is a variable and only the flag gives the
// intent away.
//
// Test files are skipped. confine_test.go legitimately runs `cmd /c mklink /J` to build
// a directory junction, which is the one thing Go's standard library cannot create; a
// test fixture is not a tool an agent can reach.
func TestNoToolStartsAShell(t *testing.T) {
	for _, dir := range []string{".", filepath.Join("..", "generator")} {
		fset := token.NewFileSet()
		//lint:ignore SA1019 this test only needs a simple file-level parse of each
		// package (no build-tag-aware file selection is required here); switching to
		// golang.org/x/tools/go/packages would add a new module dependency for no
		// behavioral gain in this audit.
		pkgs, err := parser.ParseDir(fset, dir, func(info fs.FileInfo) bool {
			return !strings.HasSuffix(info.Name(), "_test.go")
		}, parser.SkipObjectResolution)
		if err != nil {
			t.Fatalf("parsing %s: %v", dir, err)
		}

		for _, pkg := range pkgs {
			for name, file := range pkg.Files {
				ast.Inspect(file, func(n ast.Node) bool {
					call, ok := n.(*ast.CallExpr)
					if !ok || !isExecCall(call) {
						return true
					}
					checkExecCall(t, fset, name, call)
					return true
				})
			}
		}
	}
}

// isExecCall reports whether a call is exec.Command or exec.CommandContext.
//
// Matched on the selector rather than on a resolved package identity: this package
// does not alias os/exec and a test that needed full type information to answer
// "is this an exec" would be a type-checking harness rather than an audit.
func isExecCall(call *ast.CallExpr) bool {
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	pkg, ok := sel.X.(*ast.Ident)
	if !ok || pkg.Name != "exec" {
		return false
	}
	return sel.Sel.Name == "Command" || sel.Sel.Name == "CommandContext"
}

// checkExecCall reports a shell program or a shell command flag.
func checkExecCall(t *testing.T, fset *token.FileSet, file string, call *ast.CallExpr) {
	t.Helper()

	// The program is the first argument to Command and the second to CommandContext.
	// Both shapes are checked rather than the index being computed, because a wrong
	// index would silently check nothing.
	for i, arg := range call.Args {
		literal, ok := stringLiteral(arg)
		if !ok {
			continue
		}
		where := fset.Position(arg.Pos())

		if i <= 1 && shellPrograms[strings.ToLower(literal)] {
			t.Errorf("%s:%d starts %q, which interprets its argument as a command. "+
				"Every value this package execs is caller-influenced somewhere upstream, and "+
				"they are only safe because no shell splits them. Pass an argument array to "+
				"the real program instead", filepath.Base(file), where.Line, literal)
		}
		if literal == "-c" || literal == "/c" || literal == "/C" {
			t.Errorf("%s:%d passes %q to an exec call, which is how a program is told to "+
				"interpret the next argument as a command line. If this is genuinely not a "+
				"shell invocation, rename nothing and add the case here deliberately",
				filepath.Base(file), where.Line, literal)
		}
	}
}

// stringLiteral returns an argument's value when it is a plain string literal.
func stringLiteral(expr ast.Expr) (string, bool) {
	lit, ok := expr.(*ast.BasicLit)
	if !ok || lit.Kind != token.STRING {
		return "", false
	}
	// Unquoting by hand rather than with strconv: a raw string literal and an
	// interpreted one both matter here, and both are delimited by one character.
	value := lit.Value
	if len(value) < 2 {
		return "", false
	}
	return value[1 : len(value)-1], true
}
