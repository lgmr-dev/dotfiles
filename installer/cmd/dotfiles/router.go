package main

import (
	"context"
	"errors"

	"github.com/luisgui1757/dotfiles/installer/engine"
)

var importV2 = func(context.Context, setup) error {
	return errors.New("this installation has a state of release v2 (schema 1), and the import from v2.3.0 is not connected in this build; use the v2.3.0 checkout")
}

var restoreV2 = func(context.Context, setup) error {
	return errors.New("restore-v2 is not connected in this build; nothing was changed")
}

func newRouter(s setup) engine.Router { return newWiring(s).route }

func newSweep(s setup) func(context.Context, engine.Records) error { return newWiring(s).sweep }
