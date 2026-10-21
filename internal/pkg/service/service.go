package service

import (
	"context"
)

type InitFunc func(ctx context.Context) error

var modules []InitFunc

func Register(initFn InitFunc) {
	modules = append(modules, initFn)
}

func Load(ctx context.Context) error {
	for _, initFn := range modules {
		if err := initFn(ctx); err != nil {
			return err
		}
	}
	return nil
}
