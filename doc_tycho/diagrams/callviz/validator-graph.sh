#!/usr/bin/env bash

go-callvis -group pkg,type -ignore "vendor|third_party|internal/telemetry"   -format svg ~/Documents/git/tycho-energy/cmd/validator/validator.go > ~/Documents/git/tycho-energy/doc_tycho/diagrams/callviz/validator.svg