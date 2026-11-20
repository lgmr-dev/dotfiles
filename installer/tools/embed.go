// Package tools holds the tool folders. Each folder has a tool.json with the
// catalog entries of one tool, its pin files and its shell fragments.
package tools

import "embed"

// FS holds every tool.json, every pin file and every shell fragment, with
// paths relative to this folder.
//
//go:embed */*.json */shell
var FS embed.FS
