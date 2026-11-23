package builds

import (
	"context"
	"errors"
	"path/filepath"

	"github.com/lgmr-dev/dotfiles/installer/catalog"
)

func (p *Provider) prepareLatex2text(ctx context.Context, pin catalog.ArchivePin, payload string, uses []string) error {
	if pin.Latex2text.ConsoleRevision != 1 {
		return errors.New("the latex2text pin predates the UTF-8 console recipe; use a checkout with the current pins")
	}
	backend := pin.Latex2text.Backend
	backendDirectory := filepath.Join(payload, ".backend")
	if err := p.download(ctx, backend, backendDirectory); err != nil {
		return err
	}
	requirements := []privatePythonRequirement{
		{"setuptools", filepath.Join(backendDirectory, backend.File), backend.SHA256, false},
		{"pylatexenc", filepath.Join(payload, pin.File), pin.SHA256, true},
	}
	probe := "import importlib.metadata,sys; from pylatexenc.latex2text import LatexNodes2Text; assert importlib.metadata.version('pylatexenc') == sys.argv[1]; assert LatexNodes2Text().latex_to_text(r'\\alpha') == 'α'"
	return p.preparePrivatePython(ctx, pin, payload, uses, "latex2text", requirements, probe, pin.Version)
}

const latex2textConsole = `import os, sys
from pip._vendor.distlib.scripts import ScriptMaker
maker = ScriptMaker(None, os.path.dirname(sys.executable))
maker.clobber = True
maker.variants = {""}
maker.executable = sys.executable
maker.script_template = """# -*- coding: utf-8 -*-
import sys
from %(module)s import %(import_name)s
if __name__ == '__main__':
	for stream in (sys.stdin, sys.stdout, sys.stderr):
		stream.reconfigure(encoding='utf-8')
	sys.exit(%(func)s())
"""
maker.make("latex2text = pylatexenc.latex2text.__main__:main")
`
