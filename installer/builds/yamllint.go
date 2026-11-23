package builds

import (
	"context"
	"path/filepath"

	"github.com/lgmr-dev/dotfiles/installer/catalog"
)

func (p *Provider) prepareYamllint(ctx context.Context, pin catalog.ArchivePin, payload string, uses []string) error {
	requirements := []privatePythonRequirement{}
	for _, dependency := range []struct {
		name string
		pin  catalog.ArchivePin
	}{{"pathspec", pin.Yamllint.Pathspec}, {"pyyaml", pin.Yamllint.PyYAML}} {
		directory := filepath.Join(payload, "."+dependency.name)
		if err := p.download(ctx, dependency.pin, directory); err != nil {
			return err
		}
		requirements = append(requirements, privatePythonRequirement{dependency.name, filepath.Join(directory, dependency.pin.File), dependency.pin.SHA256, false})
	}
	requirements = append(requirements, privatePythonRequirement{"yamllint", filepath.Join(payload, pin.File), pin.SHA256, false})
	probe := "import importlib.metadata,sys; from yamllint import linter; from yamllint.config import YamlLintConfig; assert [importlib.metadata.version(p) for p in ('yamllint','pathspec','pyyaml')] == sys.argv[1:]; assert any(p.rule == 'key-duplicates' for p in linter.run('key: 1\\nkey: 2\\n', YamlLintConfig('{extends: relaxed}')))"
	return p.preparePrivatePython(ctx, pin, payload, uses, "yamllint", requirements, probe, pin.Version, pin.Yamllint.Pathspec.Version, pin.Yamllint.PyYAML.Version)
}
