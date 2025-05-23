// Copyright 2024 CUE Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package gotypes

import (
	"bytes"
	"fmt"
	goformat "go/format"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"

	"cuelang.org/go/cue"
	"cuelang.org/go/cue/ast"
	"cuelang.org/go/cue/build"
)

// Generate produces Go type definitions from exported CUE definitions.
// See the help text for `cue help exp gengotypes`.
func Generate(ctx *cue.Context, insts ...*build.Instance) error {
	// record which package instances have already been generated
	instDone := make(map[*build.Instance]bool)

	goPkgNamesDoneByDir := make(map[string]string)

	g := generator{generatedTypes: make(map[qualifiedPath]generatedDef)}

	// ensure we don't modify the parameter slice
	insts = slices.Clip(insts)
	for len(insts) > 0 { // we append imports to this list
		inst := insts[0]
		insts = insts[1:]
		if err := inst.Err; err != nil {
			return err
		}
		if instDone[inst] {
			continue
		}
		instDone[inst] = true

		instVal := ctx.BuildInstance(inst)
		if err := instVal.Validate(); err != nil {
			return err
		}
		g.pkg = inst
		g.emitDefs = nil
		g.pkgRoot = instVal
		g.importedAs = make(map[string]string)

		iter, err := instVal.Fields(cue.Definitions(true))
		if err != nil {
			return err
		}
		// TODO: support ignoring an entire package via a @go(-) package attribute.
		// TODO: support ignoring an entire file via a @go(-) file attribute above a package clause.
		for iter.Next() {
			sel := iter.Selector()
			if !sel.IsDefinition() {
				continue
			}
			path := cue.MakePath(sel)
			if err := g.genDef(path, iter.Value()); err != nil {
				return err
			}
		}

		// TODO: we should refuse to generate for packages which are not
		// part of the main module, as they may be inside the read-only module cache.
		for _, imp := range inst.Imports {
			if !instDone[imp] && g.importedAs[imp.ImportPath] != "" {
				insts = append(insts, imp)
			}
		}

		var buf []byte
		printf := func(format string, args ...any) {
			buf = fmt.Appendf(buf, format, args...)
		}
		printf("// Code generated by \"cue exp gengotypes\"; DO NOT EDIT.\n\n")
		goPkgName := goPkgNameForInstance(inst, instVal)
		if prev, ok := goPkgNamesDoneByDir[inst.Dir]; ok && prev != goPkgName {
			return fmt.Errorf("cannot generate two Go packages in one directory; %s and %s", prev, goPkgName)
		} else {
			goPkgNamesDoneByDir[inst.Dir] = goPkgName
		}
		printf("package %s\n\n", goPkgName)
		imported := slices.Sorted(maps.Values(g.importedAs))
		imported = slices.Compact(imported)
		if len(imported) > 0 {
			printf("import (\n")
			for _, path := range imported {
				printf("\t%q\n", path)
			}
			printf(")\n")
		}
		for _, path := range g.emitDefs {
			qpath := g.qualifiedPath(path)

			val := instVal.LookupPath(path)
			goName := goNameFromPath(path, true)
			if goName == "" {
				return fmt.Errorf("unexpected path in emitDefs: %q", qpath)
			}
			goAttr := goValueAttr(val)
			if s, _ := goAttr.String(0); s != "" {
				if s == "-" {
					continue
				}
				goName = s
			}

			g.emitDocs(goName, val.Doc())
			printf("type %s ", goName)

			// As we grab the generated source, do some sanity checks too.
			gen, ok := g.generatedTypes[qpath]
			if !ok {
				return fmt.Errorf("expected type in generatedTypes: %q", qpath)
			}
			if gen.inProgress {
				return fmt.Errorf("unexpected in-progress type in generatedTypes: %q", qpath)
			}
			if len(gen.src) == 0 {
				return fmt.Errorf("unexpected empty type in generatedTypes: %q", qpath)
			}
			buf = append(buf, gen.src...)
			printf("\n\n")
		}

		// The generated file is named after the CUE package, not the generated Go package,
		// as we can have multiple CUE packages in one directory all generating to one Go package.
		// To keep the filename short for common cases, if we are generating a CUE package
		// whose package name is implied from its import path, omit the package name element.
		basename := "cue_types_gen.go"
		ip := ast.ParseImportPath(inst.ImportPath)
		ip1 := ip
		ip1.Qualifier = ""
		ip1.ExplicitQualifier = false
		ip1 = ast.ParseImportPath(ip1.String())
		if ip.Qualifier != ip1.Qualifier {
			basename = fmt.Sprintf("cue_types_%s_gen.go", inst.PkgName)
		}
		outpath := filepath.Join(inst.Dir, basename)

		formatted, err := goformat.Source(buf)
		if err != nil {
			// Showing the generated Go code helps debug where the syntax error is.
			// This should only occur if our code generator is buggy.
			lines := bytes.Split(buf, []byte("\n"))
			var withLineNums []byte
			for i, line := range lines {
				withLineNums = fmt.Appendf(withLineNums, "% 4d: %s\n", i+1, line)
			}
			fmt.Fprintf(os.Stderr, "-- %s --\n%s\n--\n", filepath.ToSlash(outpath), withLineNums)
			return err
		}
		if err := os.WriteFile(outpath, formatted, 0o666); err != nil {
			return err
		}
	}
	return nil
}

// generator holds the state for generating Go code for one CUE package instance.
type generator struct {
	// Fields for the entire invocation, to track information about referenced definitions.

	// generatedTypes records CUE definitions which we have analyzed and translated
	// to Go type expressions.
	//
	// Analyzing types before we start emitting is useful so that, for instance,
	// a Go field can skip using a pointer to a Go type if the type is already nilable.
	generatedTypes map[qualifiedPath]generatedDef

	// Fields for each package instance.

	pkg *build.Instance

	// emitDefs records paths for the definitions we should emit as Go types.
	emitDefs []cue.Path

	// importedAs records which CUE packages need to be imported as which Go packages in the generated Go package.
	// This is collected as we emit types, given that some CUE fields and types are omitted
	// and we don't want to end up with unused Go imports.
	//
	// The keys are full CUE import paths; the values are their resulting Go import paths.
	importedAs map[string]string

	// pkgRoot is the root value of the CUE package, necessary to tell if a referenced value
	// belongs to the current package or not.
	pkgRoot cue.Value

	// Fields for each definition.

	// def tracks the generation state for a single CUE definition.
	def generatedDef
}

type qualifiedPath = string // [build.Instance.ImportPath] + " " + [cue.Path.String]

func (g *generator) qualifiedPath(path cue.Path) qualifiedPath {
	return g.pkg.ImportPath + " " + path.String()
}

// generatedDef holds information about a Go type generated for a CUE definition.
type generatedDef struct {
	// inProgress helps detect cyclic definitions and prevents emitting any Go source
	// before we are done analyzing and generating the relevant types.
	inProgress bool

	// src is the generated Go type expression source.
	src []byte
}

func (g *generatedDef) printf(format string, args ...any) {
	g.src = fmt.Appendf(g.src, format, args...)
}

type optionalStrategy int

const (
	_ optionalStrategy = iota
	// optional=zero (default); emit the Go type as-is and rely on the zero value.
	optionalZero
	// optional=nillable; emit the Go type with a pointer unless it can already
	// be compared to nil.
	optionalNillable
)

// genDef analyzes and generates a CUE definition as a Go type,
// adding it to [generator.generatedTypes] as well as [generator.emitDefs]
// to ensure that it is emitted as part of the resulting Go source.
func (g *generator) genDef(path cue.Path, val cue.Value) error {
	qpath := g.qualifiedPath(path)
	if _, ok := g.generatedTypes[qpath]; ok {
		return nil // already done or in progress
	}
	g.emitDefs = append(g.emitDefs, path)

	// When generating a Go type for a CUE definition, we may recurse into
	// this very method if a CUE field references another definition.
	// Store the current [generatedDef] in the stack so we don't lose
	// what we have generated so far, while we generate the nested type.
	oldType := g.def
	g.def = generatedDef{inProgress: true}
	g.generatedTypes[qpath] = g.def
	if err := g.emitType(val, false, optionalZero); err != nil {
		return err
	}
	g.def.inProgress = false
	g.generatedTypes[qpath] = g.def
	g.def = oldType
	return nil
}

// emitType generates a CUE value as a Go type.
// When possible, the Go type is emitted in the form of a reference.
// Otherwise, an inline Go type expression is used.
func (g *generator) emitType(val cue.Value, optional bool, optionalStg optionalStrategy) error {
	goAttr := goValueAttr(val)
	// We prefer the form @go(Name,type=pkg.Baz) as it is explicit and extensible,
	// but we are also backwards compatible with @go(Name,pkg.Baz) as emitted by `cue get go`.
	// Make sure that we don't mistake @go(,foo=bar) for a type though.
	attrType, _, _ := goAttr.Lookup(1, "type")
	if attrType == "" {
		if s, _ := goAttr.String(1); !strings.Contains(s, "=") {
			attrType = s
		}
	}
	if attrType != "" {
		pkgPath, _, ok := cutLast(attrType, ".")
		if ok {
			// For "type=foo.Name", we need to ensure that "foo" is imported.
			g.importedAs[pkgPath] = pkgPath
			// For "type=foo/bar.Name", the selector is just "bar.Name".
			// Note that this doesn't support Go packages whose name does not match
			// the last element of their import path. That seems OK for now.
			_, attrType, _ = cutLast(attrType, "/")
		}
		g.def.printf("%s", attrType)
		return nil
	}
	switch {
	case optional && optionalStg == optionalZero:
	case optional && optionalStg == optionalNillable:
		// TODO: only use a pointer when the type isn't already nillable.
		g.def.printf("*")
	}
	// TODO: support nullable types, such as `null | #SomeReference` and
	// `null | {foo: int}`.
	if g.emitTypeReference(val) {
		return nil
	}

	switch k := val.IncompleteKind(); k {
	case cue.StructKind:
		if elem := val.LookupPath(cue.MakePath(cue.AnyString)); elem.Err() == nil {
			g.def.printf("map[string]")
			if err := g.emitType(elem, false, optionalStg); err != nil {
				return err
			}
			break
		}
		// A disjunction of structs cannot be represented in Go, as it does not have sum types.
		// Fall back to a map of string to any, which is not ideal, but will work for any field.
		//
		// TODO: consider alternatives, such as:
		// * For `#StructFoo | #StructBar`, generate named types for each disjunct,
		//   and use `any` here as a sum type between them.
		// * For a disjunction of closed structs, generate a flat struct with the superset
		//   of all fields, akin to a C union.
		if op, _ := val.Expr(); op == cue.OrOp {
			g.def.printf("map[string]any")
			break
		}
		// TODO: treat a single embedding like `{[string]: int}` like we would `[string]: int`
		g.def.printf("struct {\n")
		iter, err := val.Fields(cue.Definitions(true), cue.Optional(true))
		if err != nil {
			return err
		}
		for iter.Next() {
			sel := iter.Selector()
			val := iter.Value()
			if sel.IsDefinition() {
				// TODO: why does removing [cue.Definitions] above break the tests?
				continue
			}
			cueName := sel.String()
			if sel.IsString() {
				cueName = sel.Unquoted()
			}
			cueName = strings.TrimRight(cueName, "?!")
			g.emitDocs(cueName, val.Doc())

			// We want the Go name from just this selector, even when it's not a definition.
			goName := goNameFromPath(cue.MakePath(sel), false)

			goAttr := val.Attribute("go")
			if s, _ := goAttr.String(0); s != "" {
				if s == "-" {
					continue
				}
				goName = s
			}

			optional := sel.ConstraintType()&cue.OptionalConstraint != 0
			optionalStg := optionalStg // only for this field

			// TODO: much like @go(-), support @(,optional=) when embedded in a value,
			// or attached to an entire package or file, to set a default for an entire scope.
			switch s, ok, _ := goAttr.Lookup(1, "optional"); s {
			case "zero":
				optionalStg = optionalZero
			case "nillable":
				optionalStg = optionalNillable
			default:
				if ok {
					return fmt.Errorf("unknown optional strategy %q", s)
				}
			}

			// Since CUE fields using double quotes or commas in their names are rare,
			// and the upcoming encoding/json/v2 will support field tags with name quoting,
			// we choose to ignore such fields with a clear note for now.
			if strings.ContainsAny(cueName, "\\\"`,\n") {
				g.def.printf("// CUE field %q: encoding/json does not support this field name\n\n", cueName)
				continue
			}
			g.def.printf("%s ", goName)
			if err := g.emitType(val, optional, optionalStg); err != nil {
				return err
			}
			// TODO: should we generate cuego tags like `cue:"expr"`?
			// If not, at least move the /* CUE */ comments to the end of the line.
			omitEmpty := ""
			if optional {
				omitEmpty = ",omitempty"
			}
			g.def.printf(" `json:\"%s%s\"`", cueName, omitEmpty)
			g.def.printf("\n\n")
		}
		g.def.printf("}")
	case cue.ListKind:
		// We mainly care about patterns like [...string].
		// Anything else can convert into []any as a fallback.
		g.def.printf("[]")
		elem := val.LookupPath(cue.MakePath(cue.AnyIndex))
		if !elem.Exists() {
			// TODO: perhaps mention the original type.
			g.def.printf("any /* CUE closed list */")
		} else if err := g.emitType(elem, false, optionalStg); err != nil {
			return err
		}

	case cue.NullKind:
		g.def.printf("*struct{} /* CUE null */")
	case cue.BoolKind:
		g.def.printf("bool")
	case cue.IntKind:
		g.def.printf("int64")
	case cue.FloatKind:
		g.def.printf("float64")
	case cue.StringKind:
		g.def.printf("string")
	case cue.BytesKind:
		g.def.printf("[]byte")

	case cue.NumberKind:
		// Can we do better for numbers?
		g.def.printf("any /* CUE number; int64 or float64 */")

	case cue.TopKind:
		g.def.printf("any /* CUE top */")

	// TODO: generate e.g. int8 where appropriate
	// TODO: uint64 would be marginally better than int64 for unsigned integer types

	default:
		// A disjunction of various kinds cannot be represented in Go, as it does not have sum types.
		// Also see the potential approaches in the TODO about disjunctions of structs.
		if op, _ := val.Expr(); op == cue.OrOp {
			g.def.printf("any /* CUE disjunction: %s */", k)
			break
		}
		g.def.printf("any /* TODO: IncompleteKind: %s */", k)
	}
	return nil
}

func cutLast(s, sep string) (before, after string, found bool) {
	if i := strings.LastIndex(s, sep); i >= 0 {
		return s[:i], s[i+len(sep):], true
	}
	return "", s, false
}

// goNameFromPath transforms a CUE path, such as "#foo.bar?",
// into a suitable name for a generated Go type, such as "Foo_bar".
// When defsOnly is true, all path elements must be definitions, or "" is returned.
func goNameFromPath(path cue.Path, defsOnly bool) string {
	export := true
	var sb strings.Builder
	for i, sel := range path.Selectors() {
		if defsOnly && !sel.IsDefinition() {
			return ""
		}
		if i > 0 {
			// To aid in readability, nested names are separated with underscores.
			sb.WriteString("_")
		}
		str := sel.String()
		if sel.IsString() {
			str = sel.Unquoted()
		}
		str, hidden := strings.CutPrefix(str, "_")
		if hidden {
			// If any part of the path is hidden, we are not exporting.
			export = false
		}
		// Leading or trailing characters for definitions, optional, or required
		// are not included as part of Go names.
		str = strings.TrimPrefix(str, "#")
		str = strings.TrimRight(str, "?!")
		// CUE allows quoted field names such as "foo-bar" or "123baz",
		// none of which are valid Go identifiers per https://go.dev/ref/spec#Identifiers.
		// Replace forbidden characters with underscores, like `go test` does with subtest names,
		// and add a leading "F" if the name begins with a digit.
		// TODO: this could result in name collisions; fix if it actually happens in practice.
		for i, r := range str {
			switch {
			case unicode.IsLetter(r):
				sb.WriteRune(r)
			case unicode.IsDigit(r):
				if i == 0 {
					sb.WriteRune('F')
				}
				sb.WriteRune(r)
			default:
				sb.WriteRune('_')
			}
		}
	}
	name := sb.String()
	if export {
		// Capitalize the first letter to export the name in Go.
		// https://go.dev/ref/spec#Exported_identifiers
		first, size := utf8.DecodeRuneInString(name)
		name = string(unicode.ToTitle(first)) + name[size:]
	}
	// TODO: lowercase if not exporting
	return name
}

// goValueAttr is like [cue.Value.Attribute] with the string parameter "go",
// but it supports [cue.DeclAttr] attributes as well and not just [cue.FieldAttr].
//
// TODO: surely this is a shortcoming of the method above?
func goValueAttr(val cue.Value) cue.Attribute {
	attrs := val.Attributes(cue.ValueAttr)
	for _, attr := range attrs {
		if attr.Name() == "go" {
			return attr
		}
	}
	return cue.Attribute{}
}

// goPkgNameForInstance determines what to name a Go package generated from a CUE instance.
// By default this is the CUE package name, but it can be overriden by a @go() package attribute.
func goPkgNameForInstance(inst *build.Instance, instVal cue.Value) string {
	attr := goValueAttr(instVal)
	if s, _ := attr.String(0); s != "" {
		return s
	}
	return inst.PkgName
}

// emitTypeReference attempts to generate a CUE value as a Go type via a reference,
// either to a type in the same Go package, or to a type in an imported package.
func (g *generator) emitTypeReference(val cue.Value) bool {
	// References to existing names, either from the same package or an imported package.
	root, path := val.ReferencePath()
	// TODO: surely there is a better way to check whether ReferencePath returned "no path",
	// such as a possible path.IsValid method?
	if len(path.Selectors()) == 0 {
		return false
	}
	inst := root.BuildInstance()
	// Go has no notion of qualified import paths; if a CUE file imports
	// "foo.com/bar:qualified", we import just "foo.com/bar" on the Go side.
	// TODO: deal with multiple packages existing in the same directory.
	unqualifiedPath := ast.ParseImportPath(inst.ImportPath).Unqualified().String()

	var sb strings.Builder
	if root != g.pkgRoot {
		sb.WriteString(goPkgNameForInstance(inst, root))
		sb.WriteString(".")
	}

	// As a special case, some CUE standard library types are allowed as references
	// even though they aren't definitions.
	defsOnly := true
	switch fmt.Sprintf("%s.%s", unqualifiedPath, path) {
	case "time.Duration":
		// Note that CUE represents durations as strings, but Go as int64.
		// TODO: can we do better here, such as a custom duration type?
		g.def.printf("string /* CUE time.Duration */")
		return true
	case "time.Time":
		defsOnly = false
	}

	name := goNameFromPath(path, defsOnly)
	if name == "" {
		return false // Not a path we are generating.
	}

	sb.WriteString(name)
	g.def.printf("%s", sb.String())

	// We did use a reference; if the referenced name was from another package,
	// we need to ensure that package is imported.
	// Otherwise, we need to ensure that the referenced local definition is generated.
	if root != g.pkgRoot {
		g.importedAs[inst.ImportPath] = unqualifiedPath
	} else {
		g.genDef(path, cue.Dereference(val))
	}
	return true
}

// emitDocs generates the documentation comments attached to the following declaration.
func (g *generator) emitDocs(name string, groups []*ast.CommentGroup) {
	// TODO: place the comment group starting with `// $name ...` first.
	// TODO: ensure that the Go name is used in the godoc.
	for i, group := range groups {
		if i > 0 {
			g.def.printf("//\n")
		}
		for _, line := range group.List {
			g.def.printf("%s\n", line.Text)
		}
	}
}
