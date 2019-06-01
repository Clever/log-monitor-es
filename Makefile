include golang.mk
.DEFAULT_GOAL := test # override default goal set in library makefile

.PHONY: test build $(PKGS)
SHELL := /bin/bash
PKGS := $(shell go list ./... | grep -v /vendor)
$(eval $(call golang-version-check,1.12))

test: $(PKGS)
$(PKGS): golang-test-all-deps
	$(call golang-test-all,$@)

build:
	@CGO_ENABLED=0 go build -a -installsuffix cgo

run: build
	./log-monitor-es

GLIDE_VERSION = v0.12.3
$(GOPATH)/src/github.com/Masterminds/glide:
	git clone -b $(GLIDE_VERSION) https://github.com/Masterminds/glide.git $(GOPATH)/src/github.com/Masterminds/glide

$(GOPATH)/bin/glide: $(GOPATH)/src/github.com/Masterminds/glide
	go build -o $(GOPATH)/bin/glide github.com/Masterminds/glide



install_deps: golang-dep-vendor-deps
	$(call golang-dep-vendor)