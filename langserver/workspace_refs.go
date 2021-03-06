package langserver

import (
	"context"
	"fmt"
	"github.com/saibing/bingo/langserver/internal/cache"
	"github.com/saibing/bingo/langserver/internal/util"
	"github.com/saibing/bingo/pkg/lsp"
	"log"
	"math"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/tools/go/packages"

	"github.com/saibing/bingo/langserver/internal/refs"
	"github.com/saibing/bingo/pkg/lspext"
	"github.com/sourcegraph/jsonrpc2"
)

// workspaceReferencesTimeout is the timeout used for workspace/xreferences
// calls.
const workspaceReferencesTimeout = time.Minute

func (h *LangHandler) handleWorkspaceReferences(ctx context.Context, conn jsonrpc2.JSONRPC2, req *jsonrpc2.Request, params lspext.WorkspaceReferencesParams) ([]referenceInformation, error) {
	// TODO: Add support for the cancelRequest LSP method instead of using
	// hard-coded timeouts like this here.
	//
	// See: https://github.com/Microsoft/language-server-protocol/blob/master/protocol.md#cancelRequest
	ctx, cancel := context.WithTimeout(ctx, workspaceReferencesTimeout)
	defer cancel()
	rootPath := h.FilePath(h.init.Root())

	var results = refResult{results: make([]referenceInformation, 0)}
	f := func(pkg *packages.Package) error {
		if ctx.Err() != nil {
			return ctx.Err()
		}

		// If a dirs hint is present, only look for references created in those
		// directories.
		pkgDir := ""
		if len(pkg.CompiledGoFiles) > 0 {
			pkgDir = filepath.ToSlash(filepath.Dir(pkg.CompiledGoFiles[0]))
		}
		dirs, ok := params.Hints["dirs"]
		if ok {
			found := false
			for _, dir := range dirs.([]interface{}) {
				hintDir := h.FilePath(lsp.DocumentURI(dir.(string)))
				if util.PathEqual(pkgDir, hintDir) {
					found = true
					break
				}
			}
			if !found {
				return nil
			}
		}
		
		err := h.workspaceRefsFromPkg(ctx, conn, params, pkg, rootPath, &results)
		if err != nil {
			log.Printf("workspaceRefsFromPkg: %v: %v", pkg, err)
		}
		return err
	}

	err := h.globalCache.Search(f)
	if err != nil {
		return nil, err
	}

	limit := params.Limit
	if limit <= 0 {
		// If we don't have a limit, just set it to a value we should never exceed
		limit = math.MaxInt32
	}


	r := results.results
	if len(r) > limit {
		r = r[:limit]
	}

	return r, nil
}

// workspaceRefsFromPkg collects all the references made to dependencies from
// the specified package and returns the results.
func (h *LangHandler) workspaceRefsFromPkg(ctx context.Context, conn jsonrpc2.JSONRPC2, params lspext.WorkspaceReferencesParams, pkg *packages.Package, rootPath string, results *refResult) (err error) {
	if err := ctx.Err(); err != nil {
		return err
	}

	// Compute workspace references.
	findPackage := h.getFindPackageFunc()
	cfg := &refs.Config{
		FileSet:  pkg.Fset,
		Pkg:      pkg.Types,
		PkgFiles: pkg.Syntax,
		Info:     pkg.TypesInfo,
	}
	refsErr := cfg.Refs(func(r *refs.Ref) {
		symDesc, err := defSymbolDescriptor(pkg, h.globalCache, r.Def, findPackage)
		if err != nil {
			// Log the error, and flag it as one in the trace -- but do not
			// halt execution (hopefully, it is limited to a small subset of
			// the data).
			err := fmt.Errorf("workspaceRefsFromPkg: failed to import %v: %v", r.Def.ImportPath, err)
			log.Println(err)
			return
		}

		if !symDesc.Contains(params.Query) {
			return
		}

		location := createLocationFromRange(pkg.Fset, r.Start, r.End)
		results.results = append(results.results, referenceInformation{
			Reference: location,
			Symbol:    symDesc,
		})
	})
	if refsErr != nil {
		// Trace the error, but do not consider it a true error. In many cases
		// it is a problem with the user's code, not our workspace reference
		// finding code.
		log.Println(fmt.Sprintf("workspaceRefsFromPkg: workspace refs failed: %v: %v", pkg, refsErr))
	}
	return nil
}

func defSymbolDescriptor(pkg *packages.Package, globalCache *cache.GlobalCache, def refs.Def, findPackage cache.FindPackageFunc) (*symbolDescriptor, error) {
	var err error
	defPkg, _ := pkg.Imports[def.ImportPath]
	if defPkg == nil {
		defPkg, err = findPackage(globalCache, filepath.Dir(pkg.GoFiles[0]), def.ImportPath)
		if err != nil {
			return nil, err
		}
		if defPkg == nil {
			return nil, fmt.Errorf("cannot find package for %s in %s", pkg.GoFiles[0], def.ImportPath)
		}
	}

	// NOTE: fields must be kept in sync with symbol.go:symbolEqual
	desc := &symbolDescriptor{
		Vendor:      false,
		Package:     defPkg.PkgPath,
		PackageName: def.PackageName,
		Recv:        "",
		Name:        "",
		ID:          "",
	}

	fields := strings.Fields(def.Path)
	switch {
	case len(fields) == 0:
		// reference to just a package
		desc.ID = fmt.Sprintf("%s", desc.Package)
	case len(fields) >= 2:
		desc.Recv = fields[0]
		desc.Name = fields[1]
		desc.ID = fmt.Sprintf("%s/-/%s/%s", desc.Package, desc.Recv, desc.Name)
	case len(fields) >= 1:
		desc.Name = fields[0]
		desc.ID = fmt.Sprintf("%s/-/%s", desc.Package, desc.Name)
	default:
		panic("invalid def.Path response from internal/refs")
	}
	return desc, nil
}

// refResult is a utility struct for collecting workspace reference results.
type refResult struct {
	results   []referenceInformation
}
