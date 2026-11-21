package system

import (
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
)

type aptPathFilter struct {
	include bool
	pattern *regexp.Regexp
}

var aptConfigurationName = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

func aptGlob(glob string) (*regexp.Regexp, error) {
	var expression strings.Builder
	expression.WriteString(`(?s)^`)
	for i := 0; i < len(glob); i++ {
		switch c := glob[i]; c {
		case '*':
			expression.WriteString(`.*`)
		case '?':
			expression.WriteString(`.`)
		case '\\':
			if i+1 == len(glob) {
				return nil, errors.New("glob ends with an escape")
			}
			i++
			expression.WriteString(regexp.QuoteMeta(glob[i : i+1]))
		case '[':
			end := i + 1
			if end < len(glob) && (glob[end] == '!' || glob[end] == '^') {
				end++
			}
			if end < len(glob) && glob[end] == ']' {
				end++
			}
			for end < len(glob) && glob[end] != ']' {
				end++
			}
			if end == len(glob) {
				return nil, errors.New("glob has an open bracket")
			}
			class := glob[i+1 : end]
			negate := strings.HasPrefix(class, "!") || strings.HasPrefix(class, "^")
			if negate {
				class = class[1:]
			}
			expression.WriteString("[")
			if negate {
				expression.WriteString("^")
			}
			expression.WriteString(strings.NewReplacer(`\`, `\\`, `[`, `\[`, `]`, `\]`, `^`, `\^`).Replace(class))
			expression.WriteString("]")
			i = end
		default:
			expression.WriteString(regexp.QuoteMeta(glob[i : i+1]))
		}
	}
	expression.WriteString(`$`)
	return regexp.Compile(expression.String())
}

func parseAPTPathFilters(data []byte) []aptPathFilter {
	filters := []aptPathFilter{}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		separator := strings.IndexAny(line, "= \t")
		if separator < 0 {
			continue
		}
		name := strings.TrimPrefix(line[:separator], "--")
		value := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line[separator:]), "="))
		if len(value) >= 2 && strings.HasPrefix(value, `"`) && strings.HasSuffix(value, `"`) {
			value = value[1 : len(value)-1]
		}
		if name != "path-exclude" && name != "path-include" {
			continue
		}
		pattern, err := aptGlob(value)
		if err != nil {
			continue
		}
		filters = append(filters, aptPathFilter{include: name == "path-include", pattern: pattern})
	}
	return filters
}

func readAPTPathFilters(directory string) []aptPathFilter {
	files := []string{}
	entries, err := os.ReadDir(filepath.Join(directory, "dpkg.cfg.d"))
	if err == nil {
		for _, entry := range entries {
			if aptConfigurationName.MatchString(entry.Name()) {
				files = append(files, filepath.Join(directory, "dpkg.cfg.d", entry.Name()))
			}
		}
		slices.Sort(files)
	}
	files = append(files, filepath.Join(directory, "dpkg.cfg"))
	filters := []aptPathFilter{}
	for _, file := range files {
		info, err := os.Stat(file)
		if err != nil || !info.Mode().IsRegular() || info.Size() > 1<<20 {
			continue
		}
		data, err := os.ReadFile(file)
		if err != nil {
			continue
		}
		filters = append(filters, parseAPTPathFilters(data)...)
	}
	return filters
}

func aptPathExcluded(filters []aptPathFilter, path string) bool {
	excluded := false
	for _, filter := range filters {
		if filter.pattern.MatchString(path) {
			excluded = !filter.include
		}
	}
	return excluded
}

func aptMissingPath(line string) (string, bool) {
	rest, ok := strings.CutPrefix(line, "missing")
	if !ok || !strings.HasPrefix(rest, " ") {
		return "", false
	}
	rest = strings.TrimLeft(rest, " ")
	if strings.HasPrefix(rest, "c /") {
		rest = rest[2:]
	}
	if !strings.HasPrefix(rest, "/") || strings.HasSuffix(rest, ")") && strings.Contains(rest, " (") {
		return "", false
	}
	return rest, true
}

func (a *APT) excludedMissing(line string) bool {
	path, ok := aptMissingPath(line)
	if !ok {
		return false
	}
	a.pathFiltersOnce.Do(func() {
		directory := a.DpkgConfiguration
		if directory == "" {
			directory = "/etc/dpkg"
		}
		a.pathFilters = readAPTPathFilters(directory)
	})
	return aptPathExcluded(a.pathFilters, path)
}
