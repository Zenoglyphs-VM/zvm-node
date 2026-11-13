//go:build unix

package main

import (
	"bytes"
	"debug/buildinfo"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime/debug"
	"sort"
	"strings"
)

var sourceGroups = []string{"GoFiles", "CgoFiles", "CFiles", "CXXFiles", "MFiles", "HFiles", "FFiles", "SFiles", "SwigFiles", "SwigCXXFiles", "SysoFiles"}

func listTemplate() string {
	text := `{"ImportPath":{{printf "%q" .ImportPath}},"Name":{{printf "%q" .Name}},"Dir":{{printf "%q" .Dir}},"Standard":{{.Standard}},"Incomplete":{{.Incomplete}},"Module":{{with .Module}}{"Path":{{printf "%q" .Path}},"Version":{{printf "%q" .Version}},"Main":{{.Main}},"Dir":{{printf "%q" .Dir}},"Replace":{{with .Replace}}{"Path":{{printf "%q" .Path}},"Version":{{printf "%q" .Version}},"Main":{{.Main}},"Dir":{{printf "%q" .Dir}},"Replace":null}{{else}}null{{end}}}{{else}}null{{end}},"Files":[{{$first := true}}`
	for _, group := range sourceGroups {
		text += `{{range .` + group + `}}{{if not $first}},{{end}}{{$first = false}}{"Group":"` + group + `","Path":{{printf "%q" .}}}{{end}}`
	}
	return text + `],"EmbedFiles":[{{range $i,$v := .EmbedFiles}}{{if $i}},{{end}}{{printf "%q" $v}}{{end}}]}`
}

type collector struct {
	objects            map[string][]byte
	sourceBytes        int64
	seenSource         map[string]filePin
	moduleRoots        map[string]string
	modules            map[string]*moduleObservation
	visitedDirectories map[string]bool
	noticeEntries      int
}

func newCollector() *collector {
	return &collector{objects: map[string][]byte{}, seenSource: map[string]filePin{}, moduleRoots: map[string]string{},
		modules: map[string]*moduleObservation{}, visitedDirectories: map[string]bool{}}
}

func (c *collector) object(raw []byte) (string, error) {
	digest := hash(raw)
	if _, exists := c.objects[digest]; exists {
		return digest, nil
	}
	total := len(raw)
	for _, value := range c.objects {
		total += len(value)
	}
	if len(raw) > maxNotice || total > maxObjects || len(c.objects) >= 8192 {
		return "", errors.New("notice object budget exceeded")
	}
	c.objects[digest] = raw
	return digest, nil
}

func initialNotice(raw []byte) []byte {
	end := 0
	for end < len(raw) && end < 65536 {
		if strings.ContainsRune(" \t\r\n", rune(raw[end])) {
			end++
			continue
		}
		if bytes.HasPrefix(raw[end:], []byte("//")) || raw[end] == '#' {
			next := bytes.IndexByte(raw[end:], '\n')
			if next < 0 {
				end = len(raw)
			} else {
				end += next + 1
			}
		} else if bytes.HasPrefix(raw[end:], []byte("/*")) {
			next := bytes.Index(raw[end+2:], []byte("*/"))
			if next < 0 {
				return nil
			}
			end += next + 4
		} else {
			break
		}
	}
	if end == 0 || end > 65536 {
		return nil
	}
	text := strings.ToLower(string(raw[:end]))
	if !strings.Contains(text, "copyright") && !strings.Contains(text, "license") && !strings.Contains(text, "licence") {
		return nil
	}
	return raw[:end]
}

func (c *collector) pinFile(key, root, name, group string) (filePin, error) {
	rel, err := relative(root, name)
	if err != nil {
		return filePin{}, err
	}
	cache := key + "\x00" + rel
	if previous, ok := c.seenSource[cache]; ok {
		previous.Group = group
		return previous, nil
	}
	raw, err := readRegular(root, name, maxSource)
	if err != nil {
		return filePin{}, err
	}
	c.sourceBytes += int64(len(raw))
	if c.sourceBytes > maxSources || len(c.seenSource) >= 32768 {
		return filePin{}, errors.New("source observation budget exceeded")
	}
	ids := map[string]bool{}
	for _, match := range spdx.FindAllSubmatch(initialNotice(raw), -1) {
		ids[strings.TrimSpace(string(match[1]))] = true
	}
	pin := filePin{Path: rel, Bytes: int64(len(raw)), SHA256: hash(raw), SPDX: sortedKeys(ids), Group: group}
	if header := initialNotice(raw); len(header) > 0 {
		pin.InitialNotice, err = c.object(header)
		if err != nil {
			return filePin{}, err
		}
	}
	c.seenSource[cache] = pin
	return pin, nil
}

func (c *collector) notice(key, root, name, scope string) error {
	rel, err := relative(root, name)
	if err != nil {
		return err
	}
	module := c.modules[key]
	for _, old := range module.Notices {
		if old.Path == rel {
			return nil
		}
	}
	raw, err := readRegular(root, name, maxNotice)
	if err != nil {
		return err
	}
	digest, err := c.object(raw)
	if err != nil {
		return err
	}
	module.Notices = append(module.Notices, noticePin{Path: rel, Bytes: int64(len(raw)), SHA256: digest, Scope: scope})
	return nil
}

func (c *collector) noticeDirectory(key, root, directory string, depth int) error {
	if depth > 4 {
		return errors.New("notice directory nesting exceeds bound")
	}
	if err := noLinks(root, directory); err != nil {
		return err
	}
	entries, err := os.ReadDir(directory)
	if err != nil || len(entries) > 1024 {
		return errors.New("notice directory cannot be inspected within its bound")
	}
	for _, entry := range entries {
		c.noticeEntries++
		if c.noticeEntries > 8192 {
			return errors.New("notice directory entry budget exceeded")
		}
		name := filepath.Join(directory, entry.Name())
		if entry.IsDir() {
			if err := c.noticeDirectory(key, root, name, depth+1); err != nil {
				return err
			}
		} else if err := c.notice(key, root, name, "MODULE_NOTICE_DIRECTORY"); err != nil {
			return err
		}
	}
	return nil
}

func (c *collector) ancestors(key, directory string) error {
	root := c.moduleRoots[key]
	if _, err := relative(root, filepath.Join(directory, "placeholder")); err != nil {
		return err
	}
	for {
		cache := key + "\x00" + directory
		if !c.visitedDirectories[cache] {
			if len(c.visitedDirectories) >= 8192 {
				return errors.New("ancestor directory budget exceeded")
			}
			if err := noLinks(root, directory); err != nil {
				return err
			}
			entries, err := os.ReadDir(directory)
			if err != nil || len(entries) > 8192 {
				return errors.New("source ancestor directory exceeds observation bound")
			}
			for _, entry := range entries {
				if !noticeFile.MatchString(entry.Name()) {
					continue
				}
				name := filepath.Join(directory, entry.Name())
				if entry.IsDir() {
					if err := c.noticeDirectory(key, root, name, 0); err != nil {
						return err
					}
				} else if err := c.notice(key, root, name, "SELECTED_PACKAGE_ANCESTOR"); err != nil {
					return err
				}
			}
			c.visitedDirectories[cache] = true
		}
		if filepath.Clean(directory) == filepath.Clean(root) {
			return nil
		}
		directory = filepath.Dir(directory)
	}
}

func parsePackages(raw []byte) ([]packageInput, error) {
	if len(raw) == 0 || len(raw) > maxCommand || raw[len(raw)-1] != '\n' {
		return nil, errors.New("package stream is incomplete")
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	packages, seen := []packageInput{}, map[string]bool{}
	for {
		var item json.RawMessage
		if err := decoder.Decode(&item); err != nil {
			if err == io.EOF {
				break
			}
			return nil, errors.New("package stream is malformed")
		}
		var pkg packageInput
		if err := uniqueJSON(item, &pkg); err != nil {
			return nil, err
		}
		if !portable(pkg.ImportPath) || seen[pkg.ImportPath] || pkg.Incomplete || !filepath.IsAbs(pkg.Dir) ||
			len(pkg.Files)+len(pkg.EmbedFiles) > 4096 || pkg.Name == "" {
			return nil, errors.New("incomplete or ambiguous selected package")
		}
		seen[pkg.ImportPath] = true
		packages = append(packages, pkg)
		if len(packages) > 4096 {
			return nil, errors.New("package count exceeds bound")
		}
	}
	if len(packages) == 0 {
		return nil, errors.New("empty package selection")
	}
	return packages, nil
}

func (c *collector) collect(packages []packageInput, project, goroot, sdk string) ([]packageObservation, error) {
	result, entry := []packageObservation{}, false
	for _, pkg := range packages {
		id, directory := moduleIdentity{Path: "std", Version: sdk, EffectivePath: "std", EffectiveVersion: sdk}, goroot
		if pkg.Standard {
			if pkg.Module != nil {
				return nil, errors.New("standard package has a module identity")
			}
		} else {
			var err error
			id, directory, err = identity(pkg.Module, project)
			if err != nil {
				return nil, err
			}
			if pkg.ImportPath != id.Path && !strings.HasPrefix(pkg.ImportPath, id.Path+"/") {
				return nil, errors.New("package is outside its nominal module namespace")
			}
		}
		key := moduleKey(id)
		if previous, exists := c.modules[key]; exists {
			if previous.Identity != id || c.moduleRoots[key] != directory {
				return nil, errors.New("conflicting module source identities")
			}
		} else {
			if len(c.modules) >= 1024 {
				return nil, errors.New("module count exceeds bound")
			}
			c.modules[key] = &moduleObservation{Identity: id, Notices: []noticePin{}, ReviewRequired: true}
			c.moduleRoots[key] = directory
		}
		if pkg.ImportPath == rootModule+"/cmd/zvm-node" {
			if pkg.Name != "main" || !id.Main {
				return nil, errors.New("entry is not the expected main-module command")
			}
			entry = true
		}
		if err := c.ancestors(key, pkg.Dir); err != nil {
			return nil, err
		}
		row := packageObservation{Path: pkg.ImportPath, Module: key, Sources: []filePin{}, Embedded: []filePin{}}
		seen := map[string]bool{}
		for _, file := range pkg.Files {
			allowed := false
			for _, group := range sourceGroups {
				allowed = allowed || file.Group == group
			}
			if !allowed || file.Group == "CgoFiles" || !portable(file.Path) || seen[file.Path] {
				return nil, errors.New("unsupported or duplicate selected source file")
			}
			seen[file.Path] = true
			pin, err := c.pinFile(key, directory, filepath.Join(pkg.Dir, filepath.FromSlash(file.Path)), file.Group)
			if err != nil {
				return nil, err
			}
			row.Sources = append(row.Sources, pin)
		}
		seen = map[string]bool{}
		for _, file := range pkg.EmbedFiles {
			if !portable(file) || seen[file] {
				return nil, errors.New("unsafe or duplicate embedded file")
			}
			seen[file] = true
			pin, err := c.pinFile(key, directory, filepath.Join(pkg.Dir, filepath.FromSlash(file)), "EmbedFiles")
			if err != nil {
				return nil, err
			}
			row.Embedded = append(row.Embedded, pin)
		}
		sort.Slice(row.Sources, func(i, j int) bool { return row.Sources[i].Path < row.Sources[j].Path })
		sort.Slice(row.Embedded, func(i, j int) bool { return row.Embedded[i].Path < row.Embedded[j].Path })
		result = append(result, row)
	}
	if !entry {
		return nil, errors.New("selected CLI entry is missing")
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Path < result[j].Path })
	return result, nil
}

func binaryModules(info *debug.BuildInfo, modules map[string]*moduleObservation) error {
	if info.Path != rootModule+"/cmd/zvm-node" || info.Main.Path != rootModule || info.Main.Replace != nil ||
		(info.Main.Version != "" && info.Main.Version != "(devel)") {
		return errors.New("binary main module differs")
	}
	seen := map[string]bool{}
	for _, dependency := range info.Deps {
		selected, ok := modules[dependency.Path]
		if !ok || seen[dependency.Path] {
			return errors.New("binary contains an unexpected or duplicate dependency")
		}
		seen[dependency.Path] = true
		id := selected.Identity
		path, ver := dependency.Path, dependency.Version
		if replacement := dependency.Replace; replacement != nil {
			path, ver = replacement.Path, replacement.Version
			if dependency.Path == coreModule && (ver == "" || ver == "(devel)") {
				ver = ""
			}
		}
		if dependency.Version != id.Version || path != id.EffectivePath || ver != id.EffectiveVersion {
			return errors.New("binary dependency differs from the selected source module")
		}
	}
	for name, module := range modules {
		if !module.Identity.Main && name != "std" && !seen[name] {
			return errors.New("selected dependency is absent from binary metadata")
		}
	}
	return nil
}

func readBinary(name string, profile map[string]string, modules map[string]*moduleObservation) (string, int64, error) {
	root := filepath.Dir(name)
	raw, err := readRegular(root, name, 128<<20)
	if err != nil {
		return "", 0, err
	}
	info, err := buildinfo.Read(bytes.NewReader(raw))
	if err != nil || info.GoVersion != profile["GOVERSION"] {
		return "", 0, errors.New("binary SDK metadata differs")
	}
	settings := map[string]string{}
	for _, pair := range info.Settings {
		if _, exists := settings[pair.Key]; exists {
			return "", 0, errors.New("duplicate binary build setting")
		}
		settings[pair.Key] = pair.Value
	}
	for _, name := range []string{"GOOS", "GOARCH", "CGO_ENABLED"} {
		if settings[name] != profile[name] {
			return "", 0, errors.New("binary build profile differs")
		}
	}
	if err := binaryModules(info, modules); err != nil {
		return "", 0, err
	}
	return hash(raw), int64(len(raw)), nil
}
