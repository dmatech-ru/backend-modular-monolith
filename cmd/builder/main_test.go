package main

import (
	"go/parser"
	"go/token"
	"slices"
	"testing"
)

func TestParseModules(t *testing.T) {
	available := []string{"gateway", "identity"}

	got, err := parseModules(" identity, gateway ,identity,", available)
	if err != nil || !slices.Equal(got, []string{"gateway", "identity"}) {
		t.Fatalf("parseModules = %v, %v", got, err)
	}
	if _, err := parseModules("billing", available); err == nil {
		t.Fatal("expected error for unknown module")
	}
	if _, err := parseModules(" , ", available); err == nil {
		t.Fatal("expected error for empty list")
	}
}

func TestRenderImportsIsValidGo(t *testing.T) {
	src := renderImports([]string{"gateway", "identity"})

	file, err := parser.ParseFile(token.NewFileSet(), "imports.go", src, parser.ParseComments)
	if err != nil {
		t.Fatalf("generated code does not parse: %v\n%s", err, src)
	}
	var paths []string
	for _, imp := range file.Imports {
		paths = append(paths, imp.Path.Value)
	}
	want := []string{`"dmatech.ru/internal/modules/gateway"`, `"dmatech.ru/internal/modules/identity"`}
	if !slices.Equal(paths, want) {
		t.Fatalf("imports = %v, want %v", paths, want)
	}
}
