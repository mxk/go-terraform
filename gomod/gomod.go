// Package gomod provides information about modules compiled into the binary.
package gomod

import (
	"os"
	"reflect"
	"runtime"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	_ "unsafe"
)

// File returns the file that defines function fn.
func File(fn interface{}) string {
	v := reflect.ValueOf(fn)
	if v.Kind() != reflect.Func {
		panic("gomod: not a function: " + v.Type().String())
	}
	pc := v.Pointer()
	file, _ := runtime.FuncForPC(pc).FileLine(pc)
	return file
}

// Module is a file system path to a module directory. The entire path is only
// valid on the system where the module was compiled, but it is used for
// extracting module name and version. All methods return empty strings if the
// module was not found.
type Module struct {
	path string
	base int
	at   int
}

// All returns all modules compiled into the current binary, sorted by name.
func All() []Module {
	names := names()
	all := make([]Module, 0, len(names))
	for _, name := range names {
		all = append(all, modMap[name])
	}
	return all
}

// Get returns the named module.
func Get(name string) Module {
	once.Do(load)
	return modMap[name]
}

// Root returns the module of function fn.
func Root(fn interface{}) Module {
	file := File(fn)
	if base, at := parse(file); at > 0 {
		return Get(file[base:at])
	}
	return Module{}
}

// Path returns the module root directory.
func (m Module) Path() string { return m.path }

// Name returns the module name.
func (m Module) Name() string { return m.path[m.base:m.at] }

// Version returns the module version without the 'v' prefix.
func (m Module) Version() string {
	if v := m.at + 2; v < len(m.path) {
		return m.path[v:]
	}
	return ""
}

var (
	once      sync.Once
	modMap    map[string]Module
	nameCache atomic.Value
)

//go:linkname fmd runtime.firstmoduledata
var fmd struct {
	pclntable []byte
	ftab      []struct{ entry, funcoff uintptr }
	filetab   []uint32
}

//go:linkname gostringnocopy runtime.gostringnocopy
func gostringnocopy(_ *byte) string

// load finds all modules compiled into the current binary.
func load() {
	modMap = make(map[string]Module, len(fmd.filetab)>>5)
	for _, off := range fmd.filetab {
		file := gostringnocopy(&fmd.pclntable[off])
		if base, at := parse(file); at > 0 {
			name := file[base:at]
			if _, ok := modMap[name]; !ok {
				modMap[name] = Module{trim(file, at), base, at}
			}
		}
	}
}

// names returns all module names in sorted order.
func names() []string {
	once.Do(load)
	names, _ := nameCache.Load().([]string)
	if names == nil {
		names = make([]string, 0, len(modMap))
		for name := range modMap {
			names = append(names, name)
		}
		sort.Strings(names)
		nameCache.Store(names)
	}
	return names
}

// parse returns the indices for the module name and '@' in path p.
func parse(p string) (base, at int) {
	for {
		if at = strings.IndexByte(p[base:], '@'); at < 0 {
			return 0, 0
		}
		if at += base; 0 < at && at+2 < len(p) && p[at+1] == 'v' &&
			!os.IsPathSeparator(p[at-1]) {
			if v := p[at+2]; '0' <= v && v <= '9' {
				for base = at - 2; base >= 0; base-- {
					if os.IsPathSeparator(p[base]) {
						break
					}
				}
				return base + 1, at
			}
		}
		base = at + 1
	}
}

// trim removes all path components after the module root directory.
func trim(path string, at int) string {
	for i, c := range []byte(path[at:]) {
		if os.IsPathSeparator(c) {
			return strings.TrimSuffix(path[:at+i], "+incompatible")
		}
	}
	return path
}
