package main

import (
	"code.google.com/p/go.tools/go/types"
	"github.com/kisielk/gotool"
	"honnef.co/go/importer"

	"flag"
	"fmt"
	"go/ast"
	"go/build"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

type stringSlice []string

func (s *stringSlice) String() string {
	return strings.Join(*s, ",")
}

func (s *stringSlice) Set(val string) error {
	*s = strings.Split(val, ",")
	return nil
}

var (
	packages  stringSlice
	arguments stringSlice
	returns   stringSlice
	and       bool
)

func init() {
	flag.Var(&packages, "pkgs", "Comma-separated list of packages to search for functions.")
	flag.Var(&arguments, "args", "Comma-separated list of argument types to match.")
	flag.Var(&returns, "rets", "Comma-separated list of return types to match.")
	flag.BoolVar(&and, "and", false, "Use AND instead of OR for matching functions.")

	flag.Parse()
}

func parseFile(fset *token.FileSet, fileName string) (f *ast.File, err error) {
	astFile, err := parser.ParseFile(fset, fileName, nil, 0)
	if err != nil {
		return f, fmt.Errorf("could not parse: %s", err)
	}

	return astFile, nil
}

type Type struct {
	Object   types.Object
	TypeName *types.TypeName
	Pointer  *types.Pointer
}

type Context struct {
	allImports map[string]*types.Package
	context    types.Config
	importer   *importer.Importer
}

func NewContext() *Context {
	importer := importer.New()
	importer.Config.UseGcFallback = true
	ctx := &Context{
		importer:   importer,
		allImports: importer.Imports,
		context: types.Config{
			Import: importer.Import,
		},
	}

	return ctx
}

func check(ctx *Context, name string, fset *token.FileSet, astFiles []*ast.File) (pkg *types.Package, err error) {
	return ctx.context.Check(name, fset, astFiles, nil)
}

func (ctx *Context) getObjects(paths []string) ([]types.Object, []error) {
	var errors []error
	var objects []types.Object

pathLoop:
	for _, path := range paths {
		buildPkg, err := build.Import(path, ".", 0)
		if err != nil {
			errors = append(errors, fmt.Errorf("Couldn't import %s: %s", path, err))
			continue
		}
		fset := token.NewFileSet()
		var astFiles []*ast.File
		var pkg *types.Package
		if buildPkg.Goroot {
			// TODO what if the compiled package in GoRoot is
			// outdated?
			pkg, err = types.GcImport(ctx.allImports, path)
			if err != nil {
				errors = append(errors, fmt.Errorf("Couldn't import %s: %s", path, err))
				continue
			}
		} else {
			if len(buildPkg.GoFiles) == 0 {
				errors = append(errors, fmt.Errorf("Couldn't parse %s: No (non cgo) Go files", path))
				continue pathLoop
			}
			for _, file := range buildPkg.GoFiles {
				astFile, err := parseFile(fset, filepath.Join(buildPkg.Dir, file))
				if err != nil {
					errors = append(errors, fmt.Errorf("Couldn't parse %s: %s", err))
					continue pathLoop
				}
				astFiles = append(astFiles, astFile)
			}
			pkg, err = check(ctx, path, fset, astFiles)
			if err != nil {
				errors = append(errors, fmt.Errorf("Couldn't parse %s: %s\n", path, err))
				continue pathLoop
			}
		}

		scope := pkg.Scope()
		for _, n := range scope.Names() {
			obj := scope.Lookup(n)
			objects = append(objects, obj)
		}
	}

	return objects, errors
}

// This struct only exists to work around issue 5815 (go/types: (*Func).Pkg() returns
// nil for methods from GcImport'ed packages)
type function struct {
	*types.Func
	Pkg *types.Package
}

func (ctx *Context) getFunctions(paths []string) ([]function, []error) {
	var funcs []function

	objects, errors := ctx.getObjects(paths)

	for _, obj := range objects {
		if fnc, ok := obj.(*types.Func); ok {
			funcs = append(funcs, function{fnc, obj.Pkg()})
		} else {
			typ, ok := obj.(*types.TypeName)
			if !ok {
				continue
			}

			named, ok := typ.Type().(*types.Named)
			if !ok {
				continue
			}

			for i := 0; i < named.NumMethods(); i++ {
				funcs = append(funcs, function{named.Method(i), obj.Pkg()})
			}
		}
	}

	return funcs, errors
}

func listErrors(errors []error) {
	for _, err := range errors {
		fmt.Println(err)
	}
}

func noDot(s string) string {
	index := strings.Index(s, "·")
	if index == -1 {
		return s
	}

	return s[:index]
}

func argsToString(args *types.Tuple) string {
	ret := make([]string, args.Len())
	for i := 0; i < args.Len(); i++ {
		name := noDot(args.At(i).Name())
		typ := args.At(i).Type().String()

		if len(name) == 0 {
			ret[i] = typ
		} else {
			ret[i] = name + " " + typ
		}
	}

	return strings.Join(ret, ", ")
}

func checkTypes(args *types.Tuple, types []string) (any, all bool) {
	matched := make([]bool, len(types))
	for i := 0; i < args.Len(); i++ {
		for k, toCheck := range types {
			if args.At(i).Type().String() == toCheck {
				matched[k] = true
				any = true
			}
		}
	}

	for _, b := range matched {
		if !b {
			return any, false
		}
	}

	return any, true
}

func sortedKeys(m map[string][]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func main() {
	if len(packages) == 0 {
		fmt.Fprintln(os.Stderr, "Need to specify at least one package to check.")
		flag.Usage()
		os.Exit(1)
	}

	if len(arguments)+len(returns) == 0 {
		fmt.Fprintln(os.Stderr, "Need at least one type to search for.")
		flag.Usage()
		os.Exit(1)
	}

	var typesToCheck []string
	typesToCheck = append(typesToCheck, arguments...)
	typesToCheck = append(typesToCheck, returns...)

	ctx := NewContext()
	funcs, errs := ctx.getFunctions(gotool.ImportPaths(packages))
	listErrors(errs)
	if len(ctx.importer.Fallbacks) > 0 {
		fmt.Fprintln(os.Stderr, "Relying on gc generated data for...")
		for _, path := range ctx.importer.Fallbacks {
			fmt.Fprintln(os.Stderr, path)
		}
		fmt.Fprintln(os.Stderr)
	}

	signatures := make(map[string][]string)

	for _, fnc := range funcs {
		sig, ok := fnc.Type().(*types.Signature)
		if !ok {
			// Skipping over builtins
			continue
		}

		anyArg, allArg := checkTypes(sig.Params(), arguments)
		anyRet, allRet := checkTypes(sig.Results(), returns)

		if (!and && (anyArg || anyRet)) || (and && allArg && allRet) {
			prefix := ""
			if sig.Recv() != nil {
				prefix = fmt.Sprintf("(%s %s) ", noDot(sig.Recv().Name()), sig.Recv().Type().String())
			}

			signatures[fnc.Pkg.Path()] = append(signatures[fnc.Pkg.Path()],
				fmt.Sprintf("%s%s(%s) (%s)",
					prefix,
					fnc.Name(),
					argsToString(sig.Params()),
					argsToString(sig.Results())))
		}
	}

	for _, path := range sortedKeys(signatures) {
		sigs := signatures[path]
		fmt.Println(path + ":")
		for _, sig := range sigs {
			fmt.Println("\t" + sig)
		}
		fmt.Println()
	}
}
