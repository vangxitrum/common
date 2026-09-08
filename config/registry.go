package config

import (
	"sync"

	"github.com/spf13/cobra"
	"github.com/spf13/viper"
)

// Registry tracks the configuration structs and viper instances bound to each
// command. The package-level Bind, Viper, Load and SaveConfig functions
// delegate to a default Registry, which is all most binaries need; construct
// your own with New when you need isolation, such as in tests running in
// parallel, or lifetime control over long-lived commands.
type Registry struct {
	mu      sync.Mutex
	configs map[*cobra.Command][]any
	vipers  map[*cobra.Command]*viper.Viper
}

// defaultRegistry backs the package-level functions.
var defaultRegistry = New()

// New returns an empty Registry.
func New() *Registry {
	return &Registry{
		configs: map[*cobra.Command][]any{},
		vipers:  map[*cobra.Command]*viper.Viper{},
	}
}

// Bind sets flags on a command that match the configuration struct 'config',
// and records the struct so that a later Load fills it in.
func (r *Registry) Bind(cmd *cobra.Command, config any, opts ...BindOption) {
	BindFlags(cmd.Flags(), config, opts...)

	r.mu.Lock()
	defer r.mu.Unlock()
	r.configs[cmd] = append(r.configs[cmd], config)
}

// Forget drops everything the registry holds for cmd: the bound config
// structs and the memoized viper. Long-lived processes that build commands
// dynamically should call it once a command is done, otherwise the registry
// pins each command, its flag set and its whole config struct graph for the
// lifetime of the process.
func (r *Registry) Forget(cmd *cobra.Command) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.configs, cmd)
	delete(r.vipers, cmd)
}

// boundConfigs returns the config structs bound to cmd.
func (r *Registry) boundConfigs(cmd *cobra.Command) []any {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.configs[cmd]
}

// Bind sets flags on a command that match the configuration struct 'config'
// in the default registry. It ensures that the config has all of its values
// loaded into it when Load runs.
func Bind(cmd *cobra.Command, config any, opts ...BindOption) {
	defaultRegistry.Bind(cmd, config, opts...)
}

// Forget drops everything the default registry holds for cmd.
func Forget(cmd *cobra.Command) {
	defaultRegistry.Forget(cmd)
}
