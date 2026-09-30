package bootstrap

import (
	"wagering/internal/config"

	"go.uber.org/fx"
)

var ConfigModule = fx.Module("config",
	fx.Provide(config.Load),
	fx.Invoke(func(config.Config) {}),
)

// New composes process configuration with feature modules. Feature modules
// register their resource hooks on fx.Lifecycle; Fx starts hooks in order and
// stops them in reverse order.
func New(modules ...fx.Option) *fx.App {
	options := make([]fx.Option, 0, len(modules)+1)
	options = append(options, ConfigModule)
	options = append(options, modules...)
	return fx.New(options...)
}
