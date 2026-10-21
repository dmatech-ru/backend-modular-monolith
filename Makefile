ifneq ($(filter run setup,$(firstword $(MAKECMDGOALS))),)
  ARGS := $(wordlist 2,$(words $(MAKECMDGOALS)),$(MAKECMDGOALS))
  $(eval $(ARGS):;@:)
endif

-include .env
export

export GOPRIVATE=dmatech.ru/*
export GO111MODULE=on

.PHONY: build

build:
	@go run ./cmd/builder/main.go 

dev: build
	@trap 'exit 0' 2; go run -tags dynamic ./cmd/bootstrap || true