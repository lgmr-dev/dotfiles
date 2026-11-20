package hooks

import (
	"errors"
	"path/filepath"
	"strings"
)

func desktopExecArgument(argument string) string {
	arg := strings.NewReplacer("\\", "\\\\", "\"", "\\\"", "`", "\\`", "$", "\\$", "%", "%%").Replace(argument)
	return strings.ReplaceAll("\""+arg+"\"", "\\", "\\\\")
}

func desktopEntry(name, command, icon, class string, browser bool) (string, error) {
	if !filepath.IsAbs(command) || !filepath.IsAbs(icon) || strings.ContainsAny(command+icon+name+class, "\x00\r\n") || strings.ContainsRune(command, '=') {
		return "", errors.New("invalid desktop entry path")
	}
	value := func(s string) string { return strings.ReplaceAll(s, "\\", "\\\\") }
	exec, categories, extra := desktopExecArgument(command), "Development;", ""
	if class != "" {
		extra = "StartupWMClass=" + class + "\n"
	}
	if browser {
		exec += " %u"
		categories = "Network;WebBrowser;"
		extra += "MimeType=text/html;x-scheme-handler/http;x-scheme-handler/https;\n"
	}
	return "[Desktop Entry]\nType=Application\nName=" + name + "\nExec=" + exec + "\nIcon=" + value(icon) + "\nTerminal=false\nCategories=" + categories + "\n" + extra, nil
}

func desktopXremapEntry(name, command, config string) (string, error) {
	if !filepath.IsAbs(command) || !filepath.IsAbs(config) || strings.ContainsAny(command+config, "\x00\r\n") {
		return "", errors.New("invalid xremap autostart path")
	}
	const script = `case "$XDG_SESSION_TYPE" in wayland) desktop=gnome ;; *) desktop=x11 ;; esac; config=$(readlink -f -- "$1") || config=$1; exec "$0" --desktop "$desktop" --watch=config,device --no-window-logging "$config"`
	exec := []string{}
	for _, argument := range []string{"/bin/sh", "-c", script, command, config} {
		exec = append(exec, desktopExecArgument(argument))
	}
	return "[Desktop Entry]\nType=Application\nName=" + name + " (dotfiles)\nExec=" + strings.Join(exec, " ") +
		"\nTerminal=false\nNoDisplay=true\nX-GNOME-Autostart-enabled=true\nX-GNOME-AutoRestart=true\n", nil
}
