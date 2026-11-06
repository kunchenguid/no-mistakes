package config

import (
	"fmt"
	"path"
	"strings"

	"gopkg.in/yaml.v3"
)

// Push is repository publication policy, sourced only from trusted config.
type Push struct {
	ExternalPublisher *ExternalPublisher `yaml:"external_publisher"`
}

// ExternalPublisher reserves publication when any literal path exists in the
// proposed commit. The repository supplies the explanation and delivery route.
type ExternalPublisher struct {
	Paths  []string `yaml:"paths"`
	Reason string   `yaml:"reason"`
	Remedy string   `yaml:"remedy"`
}

// UnmarshalYAML rejects misspelled policy keys rather than silently permitting
// publication. Other repository settings retain their existing parser behavior.
func (p *Push) UnmarshalYAML(node *yaml.Node) error {
	type plain Push
	var parsed plain
	if err := knownPushFields(node, "external_publisher"); err != nil {
		return err
	}
	if err := node.Decode(&parsed); err != nil {
		return err
	}
	*p = Push(parsed)
	return p.Validate()
}

func (p *ExternalPublisher) UnmarshalYAML(node *yaml.Node) error {
	type plain ExternalPublisher
	if err := knownPushFields(node, "paths", "reason", "remedy"); err != nil {
		return err
	}
	return node.Decode((*plain)(p))
}

func knownPushFields(node *yaml.Node, names ...string) error {
	if node.Kind != yaml.MappingNode {
		return fmt.Errorf("push.external_publisher policy must be a mapping")
	}
	for i := 0; i < len(node.Content); i += 2 {
		known := false
		for _, name := range names {
			known = known || node.Content[i].Value == name
		}
		if !known {
			return fmt.Errorf("unknown push policy field %q", node.Content[i].Value)
		}
	}
	return nil
}

// Validate also protects publication callers constructing Config directly.
func (p Push) Validate() error {
	gate := p.ExternalPublisher
	if gate == nil {
		return nil
	}
	if len(gate.Paths) == 0 || strings.TrimSpace(gate.Reason) == "" || strings.TrimSpace(gate.Remedy) == "" {
		return fmt.Errorf("push.external_publisher requires nonempty paths, reason, and remedy")
	}
	for _, name := range gate.Paths {
		if name == "" || name == "." || path.IsAbs(name) || path.Clean(name) != name || name == ".." || strings.HasPrefix(name, "../") || strings.ContainsAny(name, "\\\x00\r\n") {
			return fmt.Errorf("push.external_publisher.paths: %q must be a literal repository-relative path without dot segments", name)
		}
	}
	return nil
}

func copyPush(p Push) Push {
	if p.ExternalPublisher != nil {
		gate := *p.ExternalPublisher
		gate.Paths = append([]string(nil), gate.Paths...)
		p.ExternalPublisher = &gate
	}
	return p
}
