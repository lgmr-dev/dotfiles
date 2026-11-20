package system

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
)

const aptInventoryFormat = "${Package}:${Architecture}\t${Version}\t${db:Status-Want}\t${db:Status-Status}\t${db:Status-Eflag}\n"

var aptArgument = regexp.MustCompile(`^[a-z0-9][a-z0-9+.-]*(?::[a-z0-9][a-z0-9-]*)?$`)

var aptPackageIdentity = regexp.MustCompile(`^[a-z0-9][a-z0-9+.-]*:[a-z0-9][a-z0-9-]*$`)

type aptPackage struct {
	Name, Version            string
	Automatic, Held, Healthy bool
}

func (a *APT) inventory(ctx context.Context) (map[string]aptPackage, error) {
	output, err := a.Query(ctx, a.Location.DpkgQuery, "-W", "-f="+aptInventoryFormat)
	if err != nil {
		return nil, err
	}
	automatic, err := a.Query(ctx, a.Location.APTMark, "showauto")
	if err != nil {
		return nil, err
	}
	return parseAPTInventory(output, automatic)
}

func parseAPTInventory(output, automatic []byte) (map[string]aptPackage, error) {
	if len(output) > 8<<20 || len(automatic) > 8<<20 || strings.ContainsRune(string(output), 0) || strings.ContainsRune(string(automatic), 0) {
		return nil, errors.New("invalid or oversized APT inventory")
	}
	auto := map[string]bool{}
	for _, name := range strings.Fields(string(automatic)) {
		if !aptArgument.MatchString(name) || auto[name] {
			return nil, errors.New("invalid or duplicate APT automatic package")
		}
		auto[name] = true
	}
	installed := map[string]aptPackage{}
	for _, line := range strings.Split(strings.TrimSuffix(string(output), "\n"), "\n") {
		if line == "" {
			continue
		}
		fields := strings.Split(line, "\t")
		if len(fields) != 5 || !aptPackageIdentity.MatchString(fields[0]) {
			return nil, errors.New("invalid APT package database row")
		}
		if fields[3] == "not-installed" || fields[3] == "config-files" {
			continue
		}
		if _, exists := installed[fields[0]]; exists || fields[1] == "" {
			return nil, errors.New("invalid or duplicate APT package identity")
		}
		name, _, _ := strings.Cut(fields[0], ":")
		installed[fields[0]] = aptPackage{Name: fields[0], Version: fields[1],
			Automatic: auto[name] || auto[fields[0]], Held: fields[2] == "hold",
			Healthy: (fields[2] == "install" || fields[2] == "hold") && fields[3] == "installed" && fields[4] == "ok"}
	}
	return installed, nil
}

func aptIdentity(packageName, architecture string, installed map[string]aptPackage) string {
	names := []string{packageName}
	if !strings.Contains(packageName, ":") {
		names = []string{packageName + ":" + architecture, packageName + ":all"}
	}
	for _, name := range names {
		if _, ok := installed[name]; ok {
			return name
		}
	}
	return ""
}

func parseAPTCandidate(output []byte) (string, error) {
	for _, line := range strings.Split(string(output), "\n") {
		if value, ok := strings.CutPrefix(strings.TrimSpace(line), "Candidate:"); ok {
			value = strings.TrimSpace(value)
			if value == "(none)" {
				return "", nil
			}
			if value == "" || len(value) > 256 || strings.ContainsAny(value, " \t\r\x00") {
				return "", errors.New("invalid APT candidate version")
			}
			return value, nil
		}
	}
	return "", fmt.Errorf("APT policy output names no candidate version")
}
