package rollup

import (
	argus "github.com/argus-labs/argus/app"
	"github.com/argus-labs/argus/cmd/argusd/cmd"
	"github.com/argus-labs/argus/x/evm/types"
)

var _ Application = app{}

func NewApplication(cfg *Config, opts ...AppOption) Application {
	a := &app{cfg: cfg}
	for _, opt := range opts {
		opt(a)
	}
	return a
}

type app struct {
	cfg   *Config
	hooks types.EvmHooks
}

// Start does start things
//
// TODO(technicallyty): this is scrapped together, need a better configuration and setup stuff! WORLD-75
func (a app) Start() error {
	cfg := a.cfg
	encodingConfig := argus.MakeTestEncodingConfig()
	ac := cmd.AppCreator{EncCfg: encodingConfig, EvmHooks: a.hooks}
	return argus.Start(cfg.appCfg, &cfg.sCtx, cfg.cCtx, &cfg.sCfg, cfg.rollCfg, ac.NewApp)
}
