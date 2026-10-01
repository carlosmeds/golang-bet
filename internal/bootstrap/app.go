package bootstrap

import (
	"wagering/internal/config"

	"go.uber.org/fx"
)

// New composes process configuration with feature modules. Feature modules
// register their resource hooks on fx.Lifecycle; Fx starts hooks in order and
// stops them in reverse order.
func New(modules ...fx.Option) *fx.App {
	c, err := config.Load()
	if err != nil {
		return fx.New(fx.Error(err))
	}
	return NewWithConfig(c, modules...)
}

// NewWithConfig composes an already validated process configuration. Fx.Run
// uses StopTimeout to bound the complete reverse-order lifecycle shutdown.
func NewWithConfig(c config.Config, modules ...fx.Option) *fx.App {
	if err := c.Validate(); err != nil {
		return fx.New(fx.Error(err))
	}
	options := make([]fx.Option, 0, len(modules)+2)
	options = append(options, fx.Supply(c), fx.StopTimeout(c.ShutdownTimeout))
	options = append(options, modules...)
	return fx.New(options...)
}
