// Copyright 2018 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Indexed package import.
// See cmd/compile/internal/typecheck/iexport.go for the export data format.

package importer

import (
	"cmd/compile/internal/syntax"
	"cmd/compile/internal/types2"
	"encoding/binary"
	"fmt"
	"go/constant"
	"go/token"
	"io"
	"math/big"
	"sort"
	"strings"
)

type intReader struct {
	*strings.Reader
	path string
}

func (r *intReader) int64() int64 {
	i, err := binary.ReadVarint(r.Reader)
	if err != nil {
		errorf("import %q: read varint error: %v", r.path, err)
	}
	return i
}

func (r *intReader) uint64() uint64 {
	i, err := binary.ReadUvarint(r.Reader)
	if err != nil {
		errorf("import %q: read varint error: %v", r.path, err)
	}
	return i
}

// Keep this in sync with constants in iexport.go.
const (
	iexportVersionGo1_11 = 0
	iexportVersionPosCol = 1
	// TODO: before release, change this back to 2.
	iexportVersionGenerics = iexportVersionPosCol

	iexportVersionCurrent = iexportVersionGenerics
)

type ident struct {
	pkg  string
	name string
}

const predeclReserved = 32

type itag uint64

const (
	// Types
	definedType itag = iota
	pointerType
	sliceType
	arrayType
	chanType
	mapType
	signatureType
	structType
	interfaceType
	typeParamType
	instType
	unionType
)

const io_SeekCurrent = 1 // io.SeekCurrent (not defined in Go 1.4)

// iImportData imports a package from the serialized package data
// and returns the number of bytes consumed and a reference to the package.
// If the export data version is not recognized or the format is otherwise
// compromised, an error is returned.
func ImportData(imports map[string]*types2.Package, data, path string) (pkg *types2.Package, err error) {
	const currentVersion = iexportVersionCurrent
	version := int64(-1)
	defer func() {
		if e := recover(); e != nil {
			if version > currentVersion {
				err = fmt.Errorf("cannot import %q (%v), export data is newer version - update tool", path, e)
			} else {
				err = fmt.Errorf("cannot import %q (%v), possibly version skew - reinstall package", path, e)
			}
		}
	}()

	r := &intReader{strings.NewReader(data), path}

	version = int64(r.uint64())
	switch version {
	case /* iexportVersionGenerics, */ iexportVersionPosCol, iexportVersionGo1_11:
	default:
		if version > iexportVersionGenerics {
			errorf("unstable iexport format version %d, just rebuild compiler and std library", version)
		} else {
			errorf("unknown iexport format version %d", version)
		}
	}

	sLen := int64(r.uint64())
	dLen := int64(r.uint64())

	whence, _ := r.Seek(0, io_SeekCurrent)
	stringData := data[whence : whence+sLen]
	declData := data[whence+sLen : whence+sLen+dLen]
	r.Seek(sLen+dLen, io_SeekCurrent)

	p := iimporter{
		exportVersion: version,
		ipath:         path,
		version:       int(version),

		stringData:   stringData,
		pkgCache:     make(map[uint64]*types2.Package),
		posBaseCache: make(map[uint64]*syntax.PosBase),

		declData: declData,
		pkgIndex: make(map[*types2.Package]map[string]uint64),
		typCache: make(map[uint64]types2.Type),
		// Separate map for typeparams, keyed by their package and unique
		// name (name with subscript).
		tparamIndex: make(map[ident]types2.Type),
	}

	for i, pt := range predeclared {
		p.typCache[uint64(i)] = pt
	}

	pkgList := make([]*types2.Package, r.uint64())
	for i := range pkgList {
		pkgPathOff := r.uint64()
		pkgPath := p.stringAt(pkgPathOff)
		pkgName := p.stringAt(r.uint64())
		pkgHeight := int(r.uint64())

		if pkgPath == "" {
			pkgPath = path
		}
		pkg := imports[pkgPath]
		if pkg == nil {
			pkg = types2.NewPackageHeight(pkgPath, pkgName, pkgHeight)
			imports[pkgPath] = pkg
		} else {
			if pkg.Name() != pkgName {
				errorf("conflicting names %s and %s for package %q", pkg.Name(), pkgName, path)
			}
			if pkg.Height() != pkgHeight {
				errorf("conflicting heights %v and %v for package %q", pkg.Height(), pkgHeight, path)
			}
		}

		p.pkgCache[pkgPathOff] = pkg

		nameIndex := make(map[string]uint64)
		for nSyms := r.uint64(); nSyms > 0; nSyms-- {
			name := p.stringAt(r.uint64())
			nameIndex[name] = r.uint64()
		}

		p.pkgIndex[pkg] = nameIndex
		pkgList[i] = pkg
	}

	localpkg := pkgList[0]

	names := make([]string, 0, len(p.pkgIndex[localpkg]))
	for name := range p.pkgIndex[localpkg] {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		p.doDecl(localpkg, name)
	}

	// record all referenced packages as imports
	list := append(([]*types2.Package)(nil), pkgList[1:]...)
	sort.Sort(byPath(list))
	localpkg.SetImports(list)

	// package was imported completely and without errors
	localpkg.MarkComplete()

	return localpkg, nil
}

type iimporter struct {
	exportVersion int64
	ipath         string
	version       int

	stringData   string
	pkgCache     map[uint64]*types2.Package
	posBaseCache map[uint64]*syntax.PosBase

	declData    string
	pkgIndex    map[*types2.Package]map[string]uint64
	typCache    map[uint64]types2.Type
	tparamIndex map[ident]types2.Type

	interfaceList []*types2.Interface
}

func (p *iimporter) doDecl(pkg *types2.Package, name string) {
	// See if we've already imported this declaration.
	if obj := pkg.Scope().Lookup(name); obj != nil {
		return
	}

	off, ok := p.pkgIndex[pkg][name]
	if !ok {
		errorf("%v.%v not in index", pkg, name)
	}

	r := &importReader{p: p, currPkg: pkg}
	// Reader.Reset is not available in Go 1.4.
	// Use bytes.NewReader for now.
	// r.declReader.Reset(p.declData[off:])
	r.declReader = *strings.NewReader(p.declData[off:])

	r.obj(name)
}

func (p *iimporter) stringAt(off uint64) string {
	var x [binary.MaxVarintLen64]byte
	n := copy(x[:], p.stringData[off:])

	slen, n := binary.Uvarint(x[:n])
	if n <= 0 {
		errorf("varint failed")
	}
	spos := off + uint64(n)
	return p.stringData[spos : spos+slen]
}

func (p *iimporter) pkgAt(off uint64) *types2.Package {
	if pkg, ok := p.pkgCache[off]; ok {
		return pkg
	}
	path := p.stringAt(off)
	errorf("missing package %q in %q", path, p.ipath)
	return nil
}

func (p *iimporter) posBaseAt(off uint64) *syntax.PosBase {
	if posBase, ok := p.posBaseCache[off]; ok {
		return posBase
	}
	filename := p.stringAt(off)
	posBase := syntax.NewTrimmedFileBase(filename, true)
	p.posBaseCache[off] = posBase
	return posBase
}

func (p *iimporter) typAt(off uint64, base *types2.Named) types2.Type {
	if t, ok := p.typCache[off]; ok && (base == nil || !isInterface(t)) {
		return t
	}

	if off < predeclReserved {
		errorf("predeclared type missing from cache: %v", off)
	}

	r := &importReader{p: p}
	// Reader.Reset is not available in Go 1.4.
	// Use bytes.NewReader for now.
	// r.declReader.Reset(p.declData[off-predeclReserved:])
	r.declReader = *strings.NewReader(p.declData[off-predeclReserved:])
	t := r.doType(base)

	if base == nil || !isInterface(t) {
		p.typCache[off] = t
	}
	return t
}

type importReader struct {
	p           *iimporter
	declReader  strings.Reader
	currPkg     *types2.Package
	prevPosBase *syntax.PosBase
	prevLine    int64
	prevColumn  int64
}

func (r *importReader) obj(name string) {
	tag := r.byte()
	pos := r.pos()

	switch tag {
	case 'A':
		typ := r.typ()

		r.declare(types2.NewTypeName(pos, r.currPkg, name, typ))

	case 'C':
		typ, val := r.value()

		r.declare(types2.NewConst(pos, r.currPkg, name, typ, val))

	case 'F', 'G':
		var tparams []*types2.TypeParam
		if tag == 'G' {
			tparams = r.tparamList()
		}
		sig := r.signature(nil)
		sig.SetTypeParams(tparams)
		r.declare(types2.NewFunc(pos, r.currPkg, name, sig))

	case 'T', 'U':
		var tparams []*types2.TypeParam
		if tag == 'U' {
			tparams = r.tparamList()
		}

		// Types can be recursive. We need to setup a stub
		// declaration before recursing.
		obj := types2.NewTypeName(pos, r.currPkg, name, nil)
		named := types2.NewNamed(obj, nil, nil)
		named.SetTypeParams(tparams)
		r.declare(obj)

		underlying := r.p.typAt(r.uint64(), named).Underlying()
		named.SetUnderlying(underlying)

		if !isInterface(underlying) {
			for n := r.uint64(); n > 0; n-- {
				mpos := r.pos()
				mname := r.ident()
				recv := r.param()
				msig := r.signature(recv)

				// If the receiver has any targs, set those as the
				// rparams of the method (since those are the
				// typeparams being used in the method sig/body).
				targs := baseType(msig.Recv().Type()).TypeArgs()
				if targs.Len() > 0 {
					rparams := make([]*types2.TypeParam, targs.Len())
					for i := range rparams {
						rparams[i] = types2.AsTypeParam(targs.At(i))
					}
					msig.SetRParams(rparams)
				}

				named.AddMethod(types2.NewFunc(mpos, r.currPkg, mname, msig))
			}
		}

	case 'P':
		// We need to "declare" a typeparam in order to have a name that
		// can be referenced recursively (if needed) in the type param's
		// bound.
		if r.p.exportVersion < iexportVersionGenerics {
			errorf("unexpected type param type")
		}
		name0, sub := parseSubscript(name)
		tn := types2.NewTypeName(pos, r.currPkg, name0, nil)
		t := types2.NewTypeParam(tn, nil)
		if sub == 0 {
			errorf("missing subscript")
		}
		t.SetId(sub)
		// To handle recursive references to the typeparam within its
		// bound, save the partial type in tparamIndex before reading the bounds.
		id := ident{r.currPkg.Name(), name}
		r.p.tparamIndex[id] = t

		t.SetConstraint(r.typ())

	case 'V':
		typ := r.typ()

		r.declare(types2.NewVar(pos, r.currPkg, name, typ))

	default:
		errorf("unexpected tag: %v", tag)
	}
}

func (r *importReader) declare(obj types2.Object) {
	obj.Pkg().Scope().Insert(obj)
}

func (r *importReader) value() (typ types2.Type, val constant.Value) {
	typ = r.typ()

	switch b := typ.Underlying().(*types2.Basic); b.Info() & types2.IsConstType {
	case types2.IsBoolean:
		val = constant.MakeBool(r.bool())

	case types2.IsString:
		val = constant.MakeString(r.string())

	case types2.IsInteger:
		var x big.Int
		r.mpint(&x, b)
		val = constant.Make(&x)

	case types2.IsFloat:
		val = r.mpfloat(b)

	case types2.IsComplex:
		re := r.mpfloat(b)
		im := r.mpfloat(b)
		val = constant.BinaryOp(re, token.ADD, constant.MakeImag(im))

	default:
		errorf("unexpected type %v", typ) // panics
		panic("unreachable")
	}

	return
}

func intSize(b *types2.Basic) (signed bool, maxBytes uint) {
	if (b.Info() & types2.IsUntyped) != 0 {
		return true, 64
	}

	switch b.Kind() {
	case types2.Float32, types2.Complex64:
		return true, 3
	case types2.Float64, types2.Complex128:
		return true, 7
	}

	signed = (b.Info() & types2.IsUnsigned) == 0
	switch b.Kind() {
	case types2.Int8, types2.Uint8:
		maxBytes = 1
	case types2.Int16, types2.Uint16:
		maxBytes = 2
	case types2.Int32, types2.Uint32:
		maxBytes = 4
	default:
		maxBytes = 8
	}

	return
}

func (r *importReader) mpint(x *big.Int, typ *types2.Basic) {
	signed, maxBytes := intSize(typ)

	maxSmall := 256 - maxBytes
	if signed {
		maxSmall = 256 - 2*maxBytes
	}
	if maxBytes == 1 {
		maxSmall = 256
	}

	n, _ := r.declReader.ReadByte()
	if uint(n) < maxSmall {
		v := int64(n)
		if signed {
			v >>= 1
			if n&1 != 0 {
				v = ^v
			}
		}
		x.SetInt64(v)
		return
	}

	v := -n
	if signed {
		v = -(n &^ 1) >> 1
	}
	if v < 1 || uint(v) > maxBytes {
		errorf("weird decoding: %v, %v => %v", n, signed, v)
	}
	b := make([]byte, v)
	io.ReadFull(&r.declReader, b)
	x.SetBytes(b)
	if signed && n&1 != 0 {
		x.Neg(x)
	}
}

func (r *importReader) mpfloat(typ *types2.Basic) constant.Value {
	var mant big.Int
	r.mpint(&mant, typ)
	var f big.Float
	f.SetInt(&mant)
	if f.Sign() != 0 {
		f.SetMantExp(&f, int(r.int64()))
	}
	return constant.Make(&f)
}

func (r *importReader) ident() string {
	return r.string()
}

func (r *importReader) qualifiedIdent() (*types2.Package, string) {
	name := r.string()
	pkg := r.pkg()
	return pkg, name
}

func (r *importReader) pos() syntax.Pos {
	if r.p.version >= 1 {
		r.posv1()
	} else {
		r.posv0()
	}

	if (r.prevPosBase == nil || r.prevPosBase.Filename() == "") && r.prevLine == 0 && r.prevColumn == 0 {
		return syntax.Pos{}
	}

	return syntax.MakePos(r.prevPosBase, uint(r.prevLine), uint(r.prevColumn))
}

func (r *importReader) posv0() {
	delta := r.int64()
	if delta != deltaNewFile {
		r.prevLine += delta
	} else if l := r.int64(); l == -1 {
		r.prevLine += deltaNewFile
	} else {
		r.prevPosBase = r.posBase()
		r.prevLine = l
	}
}

func (r *importReader) posv1() {
	delta := r.int64()
	r.prevColumn += delta >> 1
	if delta&1 != 0 {
		delta = r.int64()
		r.prevLine += delta >> 1
		if delta&1 != 0 {
			r.prevPosBase = r.posBase()
		}
	}
}

func (r *importReader) typ() types2.Type {
	return r.p.typAt(r.uint64(), nil)
}

func isInterface(t types2.Type) bool {
	_, ok := t.(*types2.Interface)
	return ok
}

func (r *importReader) pkg() *types2.Package     { return r.p.pkgAt(r.uint64()) }
func (r *importReader) string() string           { return r.p.stringAt(r.uint64()) }
func (r *importReader) posBase() *syntax.PosBase { return r.p.posBaseAt(r.uint64()) }

func (r *importReader) doType(base *types2.Named) types2.Type {
	switch k := r.kind(); k {
	default:
		errorf("unexpected kind tag in %q: %v", r.p.ipath, k)
		return nil

	case definedType:
		pkg, name := r.qualifiedIdent()
		r.p.doDecl(pkg, name)
		return pkg.Scope().Lookup(name).(*types2.TypeName).Type()
	case pointerType:
		return types2.NewPointer(r.typ())
	case sliceType:
		return types2.NewSlice(r.typ())
	case arrayType:
		n := r.uint64()
		return types2.NewArray(r.typ(), int64(n))
	case chanType:
		dir := chanDir(int(r.uint64()))
		return types2.NewChan(dir, r.typ())
	case mapType:
		return types2.NewMap(r.typ(), r.typ())
	case signatureType:
		r.currPkg = r.pkg()
		return r.signature(nil)

	case structType:
		r.currPkg = r.pkg()

		fields := make([]*types2.Var, r.uint64())
		tags := make([]string, len(fields))
		for i := range fields {
			fpos := r.pos()
			fname := r.ident()
			ftyp := r.typ()
			emb := r.bool()
			tag := r.string()

			fields[i] = types2.NewField(fpos, r.currPkg, fname, ftyp, emb)
			tags[i] = tag
		}
		return types2.NewStruct(fields, tags)

	case interfaceType:
		r.currPkg = r.pkg()

		embeddeds := make([]types2.Type, r.uint64())
		for i := range embeddeds {
			_ = r.pos()
			embeddeds[i] = r.typ()
		}

		methods := make([]*types2.Func, r.uint64())
		for i := range methods {
			mpos := r.pos()
			mname := r.ident()

			// TODO(mdempsky): Matches bimport.go, but I
			// don't agree with this.
			var recv *types2.Var
			if base != nil {
				recv = types2.NewVar(syntax.Pos{}, r.currPkg, "", base)
			}

			msig := r.signature(recv)
			methods[i] = types2.NewFunc(mpos, r.currPkg, mname, msig)
		}

		typ := types2.NewInterfaceType(methods, embeddeds)
		r.p.interfaceList = append(r.p.interfaceList, typ)
		return typ

	case typeParamType:
		if r.p.exportVersion < iexportVersionGenerics {
			errorf("unexpected type param type")
		}
		pkg, name := r.qualifiedIdent()
		id := ident{pkg.Name(), name}
		if t, ok := r.p.tparamIndex[id]; ok {
			// We're already in the process of importing this typeparam.
			return t
		}
		// Otherwise, import the definition of the typeparam now.
		r.p.doDecl(pkg, name)
		return r.p.tparamIndex[id]

	case instType:
		if r.p.exportVersion < iexportVersionGenerics {
			errorf("unexpected instantiation type")
		}
		// pos does not matter for instances: they are positioned on the original
		// type.
		_ = r.pos()
		len := r.uint64()
		targs := make([]types2.Type, len)
		for i := range targs {
			targs[i] = r.typ()
		}
		baseType := r.typ()
		// The imported instantiated type doesn't include any methods, so
		// we must always use the methods of the base (orig) type.
		// TODO provide a non-nil *Checker
		t, _ := types2.Instantiate(nil, baseType, targs, false)
		return t

	case unionType:
		if r.p.exportVersion < iexportVersionGenerics {
			errorf("unexpected instantiation type")
		}
		terms := make([]*types2.Term, r.uint64())
		for i := range terms {
			terms[i] = types2.NewTerm(r.bool(), r.typ())
		}
		return types2.NewUnion(terms)
	}
}

func (r *importReader) kind() itag {
	return itag(r.uint64())
}

func (r *importReader) signature(recv *types2.Var) *types2.Signature {
	params := r.paramList()
	results := r.paramList()
	variadic := params.Len() > 0 && r.bool()
	return types2.NewSignature(recv, params, results, variadic)
}

func (r *importReader) tparamList() []*types2.TypeParam {
	n := r.uint64()
	if n == 0 {
		return nil
	}
	xs := make([]*types2.TypeParam, n)
	for i := range xs {
		typ := r.typ()
		xs[i] = types2.AsTypeParam(typ)
	}
	return xs
}

func (r *importReader) paramList() *types2.Tuple {
	xs := make([]*types2.Var, r.uint64())
	for i := range xs {
		xs[i] = r.param()
	}
	return types2.NewTuple(xs...)
}

func (r *importReader) param() *types2.Var {
	pos := r.pos()
	name := r.ident()
	typ := r.typ()
	return types2.NewParam(pos, r.currPkg, name, typ)
}

func (r *importReader) bool() bool {
	return r.uint64() != 0
}

func (r *importReader) int64() int64 {
	n, err := binary.ReadVarint(&r.declReader)
	if err != nil {
		errorf("readVarint: %v", err)
	}
	return n
}

func (r *importReader) uint64() uint64 {
	n, err := binary.ReadUvarint(&r.declReader)
	if err != nil {
		errorf("readUvarint: %v", err)
	}
	return n
}

func (r *importReader) byte() byte {
	x, err := r.declReader.ReadByte()
	if err != nil {
		errorf("declReader.ReadByte: %v", err)
	}
	return x
}

func baseType(typ types2.Type) *types2.Named {
	// pointer receivers are never types2.Named types
	if p, _ := typ.(*types2.Pointer); p != nil {
		typ = p.Elem()
	}
	// receiver base types are always (possibly generic) types2.Named types
	n, _ := typ.(*types2.Named)
	return n
}

func parseSubscript(name string) (string, uint64) {
	// Extract the subscript value from the type param name. We export
	// and import the subscript value, so that all type params have
	// unique names.
	sub := uint64(0)
	startsub := -1
	for i, r := range name {
		if '₀' <= r && r < '₀'+10 {
			if startsub == -1 {
				startsub = i
			}
			sub = sub*10 + uint64(r-'₀')
		}
	}
	if startsub >= 0 {
		name = name[:startsub]
	}
	return name, sub
}
