package registry

// Manifest is one decoded manifest.yaml, docs/configuration.md section 6.
// Fields keep their raw form where validation happens in this package
// (durations, URLs) so error messages can name the exact manifest value.
type Manifest struct {
	Version     int                       `yaml:"version"`
	ID          string                    `yaml:"id"`
	Name        string                    `yaml:"name"`
	Description string                    `yaml:"description"`
	Contract    string                    `yaml:"contract"`
	Handler     HandlerSpec               `yaml:"handler"`
	Secrets     map[string]string         `yaml:"secrets"`
	Operations  map[string]*OperationSpec `yaml:"operations"`
}

// HandlerSpec is the manifest's `handler` block.
type HandlerSpec struct {
	Kind   string              `yaml:"kind"`
	Entry  map[string][]string `yaml:"entry"`
	URL    string              `yaml:"url"`
	Auth   *HandlerAuth        `yaml:"auth"`
	CAFile string              `yaml:"ca_file"`
}

// HandlerAuth is `handler.auth`. http only.
type HandlerAuth struct {
	Type       string `yaml:"type"`
	Header     string `yaml:"header"`
	Credential string `yaml:"credential"`
}

// OperationSpec is one entry of the manifest's `operations` map.
type OperationSpec struct {
	Description string                `yaml:"description"`
	Mode        string                `yaml:"mode"`
	Timeout     string                `yaml:"timeout"`
	GracePeriod string                `yaml:"grace_period"`
	Privileged  bool                  `yaml:"privileged"`
	Secrets     []string              `yaml:"secrets"`
	Concurrency *ConcurrencySpec      `yaml:"concurrency"`
	Arguments   any                   `yaml:"arguments"`
	Options     map[string]string     `yaml:"options"`
	NodeFilters map[string]NodeFilter `yaml:"node_filters"`
	Entry       map[string][]string   `yaml:"entry"`
	URL         string                `yaml:"url"`
}

// ConcurrencySpec is `concurrency` on one operation.
type ConcurrencySpec struct {
	Limit *int     `yaml:"limit"`
	Key   []string `yaml:"key"`
}

// NodeFilter is one node filter, docs/configuration.md section 6.4. A node
// passes when it satisfies every field the filter sets; a filter with no
// fields accepts every `hanse:node` argument value.
type NodeFilter struct {
	OS         []string `yaml:"os"`
	Arch       []string `yaml:"arch"`
	Roles      []string `yaml:"roles"`
	Attributes []string `yaml:"attributes"`
}

// Handler kinds and operation modes.
const (
	KindExec = "exec"
	KindHTTP = "http"

	ModeSync  = "sync"
	ModeAsync = "async"

	// ContractV1 is the only handler contract version the Gateway accepts.
	ContractV1 = "hanse.handler/v1"
)
