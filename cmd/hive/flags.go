package main

import (
	"flag"
	"fmt"
	"os"
	"strings"
)

// fset wraps flag.FlagSet with a shared --net flag and positional
// splitting around a `--` terminator (so `spawn ... -- cmd` works).
type fset struct {
	*flag.FlagSet
	net     *string
	rawArgs []string
	afterDD []string // args after "--"
}

func flags(name string, args []string) *fset {
	fs := &fset{FlagSet: flag.NewFlagSet(name, flag.ContinueOnError)}
	fs.net = fs.String("net", "", "network name (default: sole local net or $HIVE_NET)")
	// Split on the first bare "--".
	for i, a := range args {
		if a == "--" {
			fs.rawArgs = args[:i]
			fs.afterDD = args[i+1:]
			return fs
		}
	}
	fs.rawArgs = args
	return fs
}

// Parse2 parses the pre-"--" args, exiting on error.
func (f *fset) Parse2() {
	if err := f.Parse(f.rawArgs); err != nil {
		os.Exit(2)
	}
}

func (f *fset) pos(i int) string {
	a := f.Args()
	if i < len(a) {
		return a[i]
	}
	return ""
}

// body joins the positionals from index i on — plus anything after "--",
// so bodies may contain words starting with dashes — into one string.
func (f *fset) body(i int) string {
	a := f.Args()
	if i > len(a) {
		i = len(a)
	}
	parts := append(append([]string{}, a[i:]...), f.afterDD...)
	return strings.Join(parts, " ")
}

func need(v, what string) (string, error) {
	if v == "" {
		return "", fmt.Errorf("missing %s", what)
	}
	return v, nil
}
