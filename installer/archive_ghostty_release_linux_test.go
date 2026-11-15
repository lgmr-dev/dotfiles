package installer

import (
	"bytes"
	"context"
	"crypto/rand"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

func TestNativeGhosttyLoadsOnOldestSupportedUbuntu(t *testing.T) {
	if os.Getenv("DOTFILES_TEST_GHOSTTY_RELEASES") != "1" {
		t.Skip("set DOTFILES_TEST_GHOSTTY_RELEASES=1 on a disposable Docker host")
	}
	images := map[string]string{"ubuntu": os.Getenv("GHOSTTY_OLDEST_UBUNTU_IMAGE"), "debian": os.Getenv("GHOSTTY_OLDEST_DEBIAN_IMAGE")}
	if !regexp.MustCompile(`^ubuntu:24\.04@sha256:[0-9a-f]{64}$`).MatchString(images["ubuntu"]) {
		t.Fatal("GHOSTTY_OLDEST_UBUNTU_IMAGE must be the digest-pinned Ubuntu 24.04 image", images["ubuntu"])
	}
	if !regexp.MustCompile(`^debian:13(-slim)?@sha256:[0-9a-f]{64}$`).MatchString(images["debian"]) {
		t.Fatal("GHOSTTY_OLDEST_DEBIAN_IMAGE must be the digest-pinned Debian 13 image", images["debian"])
	}
	docker, err := exec.LookPath("docker")
	if err != nil || !filepath.IsAbs(docker) {
		t.Fatal("the opted-in check requires Docker", err)
	}
	dpkgDeb, err := exec.LookPath("dpkg-deb")
	if err != nil || !filepath.IsAbs(dpkgDeb) {
		t.Fatal("the opted-in check requires the observed absolute dpkg-deb", err)
	}
	pin := func(distro, release string) ArchivePin {
		platform := nativePlatform(t)
		platform.Distro, platform.DistroRelease = distro, release
		pins, err := DefaultArchivePins(platform)
		if err != nil {
			t.Fatal(err)
		}
		if !pins["tool.ghostty"].GhosttyLibraries {
			t.Fatal("Ghostty must use its prepared library layout", distro, release, pins["tool.ghostty"])
		}
		return pins["tool.ghostty"]
	}
	selected := map[string]ArchivePin{"ubuntu": pin("ubuntu", "24.04"), "debian": pin("debian", "13")}
	if selected["ubuntu"].SHA256 == selected["debian"].SHA256 {
		t.Fatal("Ubuntu 24.04 must keep its own Ghostty build", selected)
	}
	catalog, err := DefaultCatalog()
	if err != nil {
		t.Fatal(err)
	}
	ghostty, _ := catalog.Resource("tool.ghostty")
	packages := []string{}
	for _, id := range ghostty.PlatformRequires["linux"] {
		library, _ := catalog.Resource(id)
		if binding := library.Bindings["linux"]; strings.HasPrefix(id, "library.") && binding.Provider == "apt" {
			packages = append(packages, binding.Package)
		}
	}
	if len(packages) < 10 {
		t.Fatal("Ghostty's catalog libraries are incomplete", packages)
	}
	payloads := map[string]string{}
	for name, pin := range selected {
		c, d, _ := archiveController(t)
		d.Pins["tool.shared"], d.Client, d.DpkgDeb = pin, nil, dpkgDeb
		dispatchApproved(t, c, Request{Schema: 1, Mode: "apply", Selected: []string{"first"}})
		if payloads[name], err = d.PayloadPath("tool.shared"); err != nil {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()
	run := func(args ...string) (string, error) {
		var output bytes.Buffer
		command := exec.CommandContext(ctx, docker, args...)
		command.Stdout, command.Stderr = &output, &output
		err := command.Run()
		return output.String(), err
	}
	install := "apt-get update -qq && DEBIAN_FRONTEND=noninteractive apt-get install -y -qq --no-install-recommends " + strings.Join(packages, " ") + " >/dev/null"
	containers := map[string]string{}
	for distro, image := range images {
		containers[distro] = "dotfiles-ghostty-" + distro + "-" + strings.ToLower(rand.Text())
		if output, err := run("run", "--detach", "--name", containers[distro],
			"--volume", payloads["ubuntu"]+":/ubuntu:ro", "--volume", payloads["debian"]+":/debian:ro",
			image, "sleep", "900"); err != nil {
			t.Fatal(output, err)
		}
		t.Cleanup(func() { _, _ = exec.Command(docker, "rm", "--force", containers[distro]).CombinedOutput() })
		if output, err := run("exec", containers[distro], "sh", "-euc", install); err != nil {
			t.Fatal(image, "cannot install Ghostty's catalog libraries", output, err)
		}
	}
	load := func(distro, payload string) (string, error) {
		return run("exec", "--env", "LD_BIND_NOW=1", "--env", "HOME=/tmp", containers[distro], "/"+payload+"/usr/bin/ghostty", "--version")
	}
	for distro, pin := range selected {
		if output, err := load(distro, distro); err != nil || !strings.Contains(output, pin.Version) {
			t.Fatal("pinned Ghostty does not load against the libraries of its oldest release", distro, output, err)
		}
	}
	if output, err := load("ubuntu", "debian"); err == nil || !strings.Contains(output, "undefined symbol: ") {
		t.Fatal("the common Ghostty build loaded on Ubuntu 24.04, so its own pin is not needed or this check cannot fail", output, err)
	}
}
