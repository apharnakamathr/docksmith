// Command docksmith is a lightweight, Docker-compatible-in-spirit
// container image store: create images from a build context, list them,
// inspect their manifests, save/load them as portable tar archives, and
// delete them with reference-counted garbage collection.
package main

import (
	"flag"
	"fmt"
	"os"
	"strings"

	"docksmith/internal/store"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "docksmith:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 {
		printUsage()
		return nil
	}

	root := os.Getenv("DOCKSMITH_ROOT")
	if root == "" {
		root = ".docksmith"
	}

	cmd, rest := args[0], args[1:]

	if cmd == "help" || cmd == "-h" || cmd == "--help" {
		printUsage()
		return nil
	}

	s, err := store.Open(root)
	if err != nil {
		return fmt.Errorf("opening store at %s: %w", root, err)
	}

	switch cmd {
	case "init":
		fmt.Printf("initialized docksmith store at %s\n", root)
		return nil
	case "create":
		return cmdCreate(s, rest)
	case "list", "ls":
		return cmdList(s)
	case "inspect":
		return cmdInspect(s, rest)
	case "save":
		return cmdSave(s, rest)
	case "load":
		return cmdLoad(s, rest)
	case "delete", "rm":
		return cmdDelete(s, rest)
	default:
		printUsage()
		return fmt.Errorf("unknown command %q", cmd)
	}
}

func printUsage() {
	fmt.Println(`docksmith - lightweight container image store

Usage:
  docksmith init
  docksmith create --tag NAME:TAG --context DIR [--cmd "cmd arg1 arg2"] [--label k=v ...]
  docksmith list
  docksmith inspect NAME:TAG
  docksmith save NAME:TAG -o out.tar
  docksmith load FILE.tar [--tag NAME:TAG]
  docksmith delete NAME:TAG

Environment:
  DOCKSMITH_ROOT   store directory (default: ./.docksmith)`)
}

func cmdCreate(s *store.Store, args []string) error {
	fs := flag.NewFlagSet("create", flag.ContinueOnError)
	tag := fs.String("tag", "", "image tag, e.g. myapp:latest")
	context := fs.String("context", ".", "build context directory")
	cmdStr := fs.String("cmd", "", "command to run, e.g. \"/bin/app --serve\"")
	var labels multiFlag
	fs.Var(&labels, "label", "label in key=value form (repeatable)")
	if err := fs.Parse(reorderFlags(fs, args)); err != nil {
		return err
	}
	if *tag == "" {
		return fmt.Errorf("--tag is required")
	}

	var command []string
	if *cmdStr != "" {
		command = strings.Fields(*cmdStr)
	}
	labelMap, err := labels.toMap()
	if err != nil {
		return err
	}

	m, err := s.CreateImage(*tag, *context, command, labelMap)
	if err != nil {
		return err
	}
	fmt.Printf("created %s\n  manifest: %s\n  layers:   %d\n", *tag, digestShort(m.Config.Digest), len(m.Layers))
	for _, l := range m.Layers {
		fmt.Printf("    - %s (%d bytes)\n", digestShort(l.Digest), l.Size)
	}
	return nil
}

func cmdList(s *store.Store) error {
	images, err := s.ListImages()
	if err != nil {
		return err
	}
	if len(images) == 0 {
		fmt.Println("no images")
		return nil
	}
	fmt.Printf("%-24s %-16s %-8s %-20s\n", "TAG", "DIGEST", "LAYERS", "CREATED")
	for _, img := range images {
		fmt.Printf("%-24s %-16s %-8d %-20s\n",
			img.Tag, digestShort(img.ManifestDigest), img.LayerCount, img.Created.Format("2006-01-02 15:04:05"))
	}
	return nil
}

func cmdInspect(s *store.Store, args []string) error {
	if len(args) != 1 {
		return fmt.Errorf("usage: docksmith inspect NAME:TAG")
	}
	m, err := s.InspectImage(args[0])
	if err != nil {
		return err
	}
	return printJSON(m)
}

func cmdSave(s *store.Store, args []string) error {
	fs := flag.NewFlagSet("save", flag.ContinueOnError)
	out := fs.String("o", "", "output tar path")
	if err := fs.Parse(reorderFlags(fs, args)); err != nil {
		return err
	}
	if fs.NArg() != 1 || *out == "" {
		return fmt.Errorf("usage: docksmith save NAME:TAG -o out.tar")
	}
	if err := s.SaveImage(fs.Arg(0), *out); err != nil {
		return err
	}
	fmt.Printf("saved %s -> %s\n", fs.Arg(0), *out)
	return nil
}

func cmdLoad(s *store.Store, args []string) error {
	fs := flag.NewFlagSet("load", flag.ContinueOnError)
	tag := fs.String("tag", "", "tag to assign the loaded image")
	if err := fs.Parse(reorderFlags(fs, args)); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return fmt.Errorf("usage: docksmith load FILE.tar [--tag NAME:TAG]")
	}
	m, err := s.LoadImage(fs.Arg(0), *tag)
	if err != nil {
		return err
	}
	fmt.Printf("loaded %s\n  layers: %d\n", fs.Arg(0), len(m.Layers))
	if *tag != "" {
		fmt.Printf("  tagged: %s\n", *tag)
	}
	return nil
}

func cmdDelete(s *store.Store, args []string) error {
	if len(args) != 1 {
		return fmt.Errorf("usage: docksmith delete NAME:TAG")
	}
	if err := s.DeleteImage(args[0]); err != nil {
		return err
	}
	fmt.Printf("deleted %s\n", args[0])
	return nil
}

func digestShort(d string) string {
	if len(d) > 19 { // "sha256:" + 12 hex chars
		return d[:19]
	}
	return d
}

// reorderFlags rearranges args so that every recognized flag (and its
// value) comes before any positional argument, regardless of the order the
// user actually typed them in. Go's flag package otherwise stops parsing
// flags at the first positional token, which makes commands like
// `docksmith save mytag -o out.tar` fail in a way that's surprising for a
// CLI. Every flag defined on fs in this program takes a value (there are
// no bare boolean switches), so a recognized "-name" token always consumes
// exactly one following token as its value.
func reorderFlags(fs *flag.FlagSet, args []string) []string {
	var flags, positional []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		if len(a) > 1 && a[0] == '-' {
			name := strings.TrimLeft(a, "-")
			if eq := strings.IndexByte(name, '='); eq != -1 {
				name = name[:eq]
			}
			if fs.Lookup(name) != nil {
				if strings.Contains(a, "=") {
					flags = append(flags, a)
				} else if i+1 < len(args) {
					flags = append(flags, a, args[i+1])
					i++
				} else {
					flags = append(flags, a)
				}
				continue
			}
		}
		positional = append(positional, a)
	}
	return append(flags, positional...)
}

// multiFlag collects repeated --label k=v flags.
type multiFlag []string

func (m *multiFlag) String() string { return strings.Join(*m, ",") }
func (m *multiFlag) Set(v string) error {
	*m = append(*m, v)
	return nil
}
func (m multiFlag) toMap() (map[string]string, error) {
	if len(m) == 0 {
		return nil, nil
	}
	out := make(map[string]string, len(m))
	for _, kv := range m {
		parts := strings.SplitN(kv, "=", 2)
		if len(parts) != 2 {
			return nil, fmt.Errorf("invalid --label %q, expected key=value", kv)
		}
		out[parts[0]] = parts[1]
	}
	return out, nil
}
