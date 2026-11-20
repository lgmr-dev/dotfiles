// Package catalog loads the tool folders: the resources, their platform
// bindings, configuration targets, shell fragments and pins.
package catalog

// Resource keeps the v2 resource fields and meanings. A capability is a menu
// entry: it has a category, a menu position in Order, and names its members
// in Requires.
type Resource struct {
	ID                    string              `json:"id"`
	Name                  string              `json:"name"`
	Description           string              `json:"description,omitempty"`
	Capability            bool                `json:"capability,omitempty"`
	Category              string              `json:"category,omitempty"`
	Order                 int                 `json:"order,omitempty"`
	Platforms             []string            `json:"platforms,omitempty"`
	PlatformArchitectures map[string][]string `json:"platform_architectures,omitempty"`
	Requires              []string            `json:"requires,omitempty"`
	PlatformRequires      map[string][]string `json:"platform_requires,omitempty"`
	Action                string              `json:"action,omitempty"`
	Scope                 string              `json:"scope,omitempty"`
	Retain                bool                `json:"retain,omitempty"`
	Shared                bool                `json:"shared,omitempty"`
	Bindings              map[string]Binding  `json:"bindings,omitempty"`
	ReplanAfter           bool                `json:"replan_after,omitempty"`
}

// Binding names the package route of a resource on one system.
type Binding struct {
	Provider string `json:"provider"`
	Package  string `json:"package"`
}

// Target is one configuration destination of a resource, with a source path
// relative to the checkout.
type Target struct {
	Resource  string   `json:"resource"`
	Source    string   `json:"source"`
	Folder    string   `json:"folder"`
	Path      string   `json:"path"`
	Platforms []string `json:"platforms,omitempty"`
	Directory bool     `json:"directory,omitempty"`
	Mode      string   `json:"mode,omitempty"`
}

// Fragment is one shell initialization source of a resource, relative to its
// tool folder. Load sets Folder to the name of that tool folder.
type Fragment struct {
	Resource string `json:"resource"`
	Shell    string `json:"shell"`
	Source   string `json:"source"`
	Folder   string `json:"-"`
}

// Tool is the content of one tool.json.
type Tool struct {
	Schema    int        `json:"schema"`
	Resources []Resource `json:"resources"`
	Targets   []Target   `json:"targets,omitempty"`
	Shell     []Fragment `json:"shell,omitempty"`
}
