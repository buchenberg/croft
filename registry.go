package containerd

import (
	"fmt"

	"github.com/buchenberg/shepherd-kernel-go"
)

// OpenSandbox registration: importing this package makes the containerd
// backend resolvable by name through shepherd.OpenSandbox(BackendName, Config),
// the same database/sql-driver convention the in-core git backend follows
// (sandbox_git.go). A host that wires its own construction (NewWithBackend over
// an existing containerd client) does not need this path, but the registry is
// how config-driven backend selection — yaah's workspace.backend key — reaches
// this backend without importing a constructor.
//
// Since v0.1.5 the nested module pins core v0.10.0, which is what carries the
// registry; before that, hosts had to register a factory themselves because the
// adapter could not see RegisterSandbox.
//
// Registering a second factory for the same name panics, so a host that also
// calls shepherd.RegisterSandbox(BackendName, ...) must not: import this
// package and the registration is done.
func init() {
	shepherd.RegisterSandbox(BackendName, func(cfg any) (shepherd.Sandbox, error) {
		c, ok := cfg.(Config)
		if !ok {
			if cfg == nil {
				return nil, fmt.Errorf("containerd sandbox: OpenSandbox requires a Config; nil is not one — a backend cannot be chosen by omission")
			}
			return nil, fmt.Errorf("containerd sandbox: OpenSandbox configuration is Config, got %T", cfg)
		}
		return New(c), nil
	})
}
