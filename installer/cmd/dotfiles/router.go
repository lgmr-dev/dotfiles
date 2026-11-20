package main

import (
	"context"
	"errors"

	"github.com/luisgui1757/dotfiles/installer/catalog"
	"github.com/luisgui1757/dotfiles/installer/engine"
)

var importV2 = func(context.Context, setup) error {
	return errors.New("this installation has a state of release v2 (schema 1), and the import from v2.3.0 is not connected in this build; use the v2.3.0 checkout")
}

var restoreV2 = func(context.Context, setup) error {
	return errors.New("restore-v2 is not connected in this build; nothing was changed")
}

func newRouter(setup) engine.Router {
	return func(r catalog.Resource) (engine.Provider, error) {
		return nil, errors.New("provider wiring is not connected for " + r.ID)
	}
}

func newSweep(setup) func(context.Context, engine.Records) error {
	return nil
}
